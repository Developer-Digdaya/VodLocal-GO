// Command loadtest drives concurrent HLS playback sessions against a running
// vodlocal server, which is how the streaming endpoints are validated for a fleet
// of tablets before a release.
//
// Example: validate 100 tablets for a minute against a local server, ramping the
// clients up over ten seconds:
//
//	go run ./cmd/loadtest -base http://127.0.0.1:8080 -playable 7 \
//	    -clients 100 -duration 60s -ramp 10s -timeout 10s
//
// The command prints a text summary and, as its last line, one JSON object with
// the same numbers so a release script can read them. It exits with status 1
// when the run failed or when any request errored, and with status 2 when the
// flags were wrong.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"vodlocal/internal/loadgen"
)

// exit statuses of the command.
const (
	exitOK      = 0
	exitFailed  = 1 // the run failed or reported errors
	exitUsage   = 2 // bad flags
	maxListedSC = 12
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the flags, runs the load test and prints the result. It is kept
// separate from main so it stays readable and testable.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var (
		base       = flags.String("base", "http://127.0.0.1:8080", "base URL server, mis. http://127.0.0.1:8080")
		playable   = flags.Int64("playable", 0, "ID playable untuk membentuk URL master")
		master     = flags.String("master", "", `path master playlist langsung (menimpa -playable), mis. /api/v1/playables/7/master.m3u8`)
		token      = flags.String("token", "", "Bearer token device (opsional untuk /media)")
		clients    = flags.Int("clients", 100, "jumlah klien paralel")
		duration   = flags.Duration("duration", 30*time.Second, "durasi uji")
		ramp       = flags.Duration("ramp", 10*time.Second, "waktu menaikkan klien secara bertahap")
		variant    = flags.String("variant", "", "substring URI varian yang dipakai (kosong = varian pertama)")
		reqTimeout = flags.Duration("timeout", 10*time.Second, "timeout per request")
	)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "loadtest: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}

	cfg := loadgen.Config{
		BaseURL:    strings.TrimSpace(*base),
		PlayableID: *playable,
		MasterPath: strings.TrimSpace(*master),
		Token:      strings.TrimSpace(*token),
		Clients:    *clients,
		Duration:   *duration,
		Ramp:       *ramp,
		Timeout:    *reqTimeout,
		Variant:    *variant,
	}
	if cfg.MasterPath == "" && cfg.PlayableID <= 0 {
		fmt.Fprintln(stderr, "loadtest: -playable must be a positive ID when -master is not given")
		return exitUsage
	}
	if cfg.Ramp < 0 {
		fmt.Fprintf(stderr, "loadtest: -ramp must not be negative, got %s\n", cfg.Ramp)
		return exitUsage
	}
	if cfg.Timeout < 0 {
		fmt.Fprintf(stderr, "loadtest: -timeout must not be negative, got %s\n", cfg.Timeout)
		return exitUsage
	}

	// Ctrl+C ends the run the same way the duration does: with a filled report.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cfg.Ramp >= cfg.Duration {
		fmt.Fprintf(stderr, "loadtest: warning: -ramp %s is not shorter than -duration %s, so the last clients will never start\n",
			cfg.Ramp, cfg.Duration)
	}

	report, err := loadgen.Run(ctx, cfg)
	if err != nil {
		// The report holds nothing useful when the configuration was rejected;
		// the JSON line still reports the failure for a script to read.
		printJSON(stdout, cfg, report, err)
		fmt.Fprintf(stderr, "loadtest: %v\n", err)
		return exitFailed
	}

	printSummary(stdout, cfg, report)
	printJSON(stdout, cfg, report, nil)
	if report.Errors > 0 {
		fmt.Fprintf(stderr, "loadtest: %d of %d requests failed\n", report.Errors, report.Requests)
		return exitFailed
	}
	return exitOK
}

// ms renders a duration as milliseconds with microsecond precision, the unit
// used in the summary and in the JSON line.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// printSummary writes the human readable part of the result.
func printSummary(w io.Writer, cfg loadgen.Config, report loadgen.Report) {
	target := masterPath(cfg)

	fmt.Fprintf(w, "loadtest: %d klien, durasi %s, ramp %s\n", cfg.Clients, cfg.Duration, cfg.Ramp)
	fmt.Fprintf(w, "target : %s%s\n", strings.TrimSuffix(cfg.BaseURL, "/"), target)
	if cfg.Variant != "" {
		fmt.Fprintf(w, "variant: %s\n", cfg.Variant)
	}
	if cfg.Token != "" {
		fmt.Fprintln(w, "token  : dikirim sebagai Bearer")
	}
	fmt.Fprintf(w, "hasil  : %d request, %d error, %d byte, %s\n",
		report.Requests, report.Errors, report.Bytes, formatBytes(report.Bytes))
	fmt.Fprintf(w, "waktu  : %s (%.3f Mbps)\n", roundTo(report.Duration, time.Millisecond), report.ThroughputMbps)
	fmt.Fprintf(w, "latensi: p50 %.3f ms, p95 %.3f ms, p99 %.3f ms\n",
		ms(report.P50), ms(report.P95), ms(report.P99))
	fmt.Fprintf(w, "status : %s\n", formatStatusCodes(report.StatusCodes))
}

// printJSON writes the machine readable single line result.
func printJSON(w io.Writer, cfg loadgen.Config, report loadgen.Report, runErr error) {
	out := struct {
		Base           string        `json:"base"`
		Master         string        `json:"master"`
		Variant        string        `json:"variant,omitempty"`
		Clients        int           `json:"clients"`
		DurationMS     float64       `json:"duration_ms"`
		RampMS         float64       `json:"ramp_ms"`
		Requests       int64         `json:"requests"`
		Errors         int64         `json:"errors"`
		Bytes          int64         `json:"bytes"`
		ThroughputMbps float64       `json:"throughput_mbps"`
		P50MS          float64       `json:"p50_ms"`
		P95MS          float64       `json:"p95_ms"`
		P99MS          float64       `json:"p99_ms"`
		StatusCodes    map[int]int64 `json:"status_codes"`
		OK             bool          `json:"ok"`
		Error          string        `json:"error,omitempty"`
	}{
		Base:           cfg.BaseURL,
		Master:         masterPath(cfg),
		Variant:        cfg.Variant,
		Clients:        report.Clients,
		DurationMS:     ms(cfg.Duration),
		RampMS:         ms(cfg.Ramp),
		Requests:       report.Requests,
		Errors:         report.Errors,
		Bytes:          report.Bytes,
		ThroughputMbps: report.ThroughputMbps,
		P50MS:          ms(report.P50),
		P95MS:          ms(report.P95),
		P99MS:          ms(report.P99),
		StatusCodes:    report.StatusCodes,
		OK:             runErr == nil && report.Errors == 0,
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}
	if out.StatusCodes == nil {
		out.StatusCodes = map[int]int64{}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		// The struct above only holds strings, numbers and a map of numbers, so
		// this cannot fail; report it rather than printing a half line.
		fmt.Fprintf(w, "{\"ok\":false,\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", encoded)
}

// masterPath returns the master playlist path the run used.
func masterPath(cfg loadgen.Config) string {
	if cfg.MasterPath != "" {
		return cfg.MasterPath
	}
	return fmt.Sprintf("/api/v1/playables/%d/master.m3u8", cfg.PlayableID)
}

// formatStatusCodes renders the status code counters sorted by code.
func formatStatusCodes(codes map[int]int64) string {
	if len(codes) == 0 {
		return "-"
	}
	keys := make([]int, 0, len(codes))
	for code := range codes {
		keys = append(keys, code)
	}
	sort.Ints(keys)

	parts := make([]string, 0, len(keys))
	for i, code := range keys {
		if i == maxListedSC {
			parts = append(parts, fmt.Sprintf("... %d kode lain", len(keys)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%d: %d", code, codes[code]))
	}
	return strings.Join(parts, ", ")
}

// formatBytes renders a byte count with a binary unit.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, name := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// roundTo truncates d to the resolution of step.
func roundTo(d, step time.Duration) time.Duration {
	if step <= 0 {
		return d
	}
	return d.Round(step)
}
