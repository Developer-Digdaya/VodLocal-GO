// Command devseed mengisi database dan media root dengan data sintetis supaya
// server bisa diuji end-to-end (termasuk uji beban 100 klien) tanpa memanggil
// API provider asli.
//
// Contoh, dengan nilai default (2 series, 2 episode, 3 segmen 64 KiB):
//
//	go run ./cmd/devseed -db data/app.db -media data/media
//
// Setelah selesai, generation aktif dicetak bersama contoh URL /media/... yang
// bisa langsung diminta ke server.
//
// Jalur default mengikuti environment VODLOCAL_DB_PATH dan VODLOCAL_MEDIA_ROOT, lalu
// jatuh ke data/app.db dan data/media. Perintah keluar dengan status 1 bila
// terjadi kegagalan (termasuk flag yang salah).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"vodlocal/internal/devseed"
)

// Environment variable dan nilai default yang dipakai command ini.
const (
	envDBPath        = "VODLOCAL_DB_PATH"
	envMediaRoot     = "VODLOCAL_MEDIA_ROOT"
	defaultDBPath    = "data/app.db"
	defaultMediaRoot = "data/media"

	// exitOK dan exitFailed adalah satu-satunya status keluar command.
	exitOK     = 0
	exitFailed = 1

	// maxListedPlaylists membatasi jumlah playlist yang dicetak.
	maxListedPlaylists = 5
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run membaca flag, menjalankan seed dan mencetak ringkasan. Nilai baliknya
// adalah status keluar command; dipisah dari main supaya bisa diuji.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("devseed", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var (
		db          = flags.String("db", envOr(envDBPath, defaultDBPath), "path file SQLite")
		mediaRoot   = flags.String("media", envOr(envMediaRoot, defaultMediaRoot), "root direktori media")
		titles      = flags.Int("titles", 2, "jumlah series sintetis")
		episodes    = flags.Int("episodes", 2, "jumlah episode per series")
		segments    = flags.Int("segments", 3, "jumlah segmen per rendition")
		segmentSize = flags.Int("segment-size", 64*1024, "ukuran satu segmen dalam byte")
		quality     = flags.String("quality", "480p", "label kualitas rendition")
		provider    = flags.String("provider", "dev", "kode provider sintetis")
	)
	if err := flags.Parse(args); err != nil {
		// flag.ContinueOnError sudah mencetak pesan dan petunjuk pemakaian.
		return exitFailed
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "devseed: argumen tidak dikenal %q\n", flags.Arg(0))
		return exitFailed
	}

	cfg := devseed.Config{
		DBPath:      strings.TrimSpace(*db),
		MediaRoot:   strings.TrimSpace(*mediaRoot),
		Titles:      *titles,
		Episodes:    *episodes,
		Segments:    *segments,
		SegmentSize: *segmentSize,
		Quality:     strings.TrimSpace(*quality),
		Provider:    strings.TrimSpace(*provider),
	}

	res, err := devseed.Run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "devseed: %v\n", err)
		return exitFailed
	}

	printSummary(stdout, cfg, res)
	return exitOK
}

// printSummary mencetak ringkasan seed: generation, jumlah data dan contoh URL
// media yang bisa langsung diminta ke server.
func printSummary(w io.Writer, cfg devseed.Config, res devseed.Result) {
	fmt.Fprintf(w, "generation : %s (aktif)\n", res.Generation)
	fmt.Fprintf(w, "database   : %s\n", cfg.DBPath)
	fmt.Fprintf(w, "media      : %s\n", cfg.MediaRoot)
	fmt.Fprintf(w, "provider   : %s (id %d)\n", cfg.Provider, res.ProviderID)
	fmt.Fprintf(w, "title      : %d\n", len(res.TitleIDs))
	fmt.Fprintf(w, "playable   : %d\n", len(res.PlayableIDs))

	if len(res.PlaylistPaths) == 0 {
		return
	}

	fmt.Fprintln(w, "playlist   :")
	for i, rel := range res.PlaylistPaths {
		if i == maxListedPlaylists {
			fmt.Fprintf(w, "  ... %d playlist lain\n", len(res.PlaylistPaths)-i)
			break
		}
		fmt.Fprintf(w, "  /media/%s/%s\n", res.Generation, slashify(rel))
	}

	fmt.Fprintf(w, "contoh URL : /media/%s/%s\n", res.Generation, slashify(res.PlaylistPaths[0]))
}

// slashify menormalkan pemisah path menjadi "/" supaya URL yang dicetak sama di
// Windows maupun POSIX.
func slashify(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/")
}

// envOr mengembalikan nilai environment key bila terisi, atau def.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
