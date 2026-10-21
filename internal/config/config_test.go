package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDramahitsBaseURL adalah URL valid yang dipakai test. Nilai ini bukan
// default aplikasi: VODLOCAL_DRAMAHITS_BASE_URL wajib diisi saat runtime.
const testDramahitsBaseURL = "https://dramahits.example/api/v1"

// mapEnv membuat fungsi getenv dari map, meniru os.Getenv. URL Dramahits
// disisipkan sebagai nilai valid agar test lain tidak perlu mengulanginya;
// nilai di map eksplisit tetap menimpa nilai bawaan ini.
func mapEnv(env map[string]string) func(string) string {
	merged := make(map[string]string, len(env)+1)
	merged[envDramahitsBaseURL] = testDramahitsBaseURL
	for key, value := range env {
		merged[key] = value
	}
	return func(key string) string {
		return merged[key]
	}
}

// absDefault mengembalikan nilai default path setelah di-resolve filepath.Abs,
// karena MediaRoot/ArtworkRoot selalu dipulangkan dalam bentuk absolut.
func absDefault(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs(%q) error: %v", path, err)
	}
	return abs
}

// TestLoadFromDefaults memastikan map kosong menghasilkan seluruh nilai default.
func TestLoadFromDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(mapEnv(map[string]string{}))
	if err != nil {
		t.Fatalf("LoadFrom(map kosong) error tak terduga: %v", err)
	}

	want := Config{
		HTTPAddr:                ":8080",
		DBPath:                  "data/app.db",
		MediaRoot:               absDefault(t, "data/media"),
		ArtworkRoot:             absDefault(t, "data/media/artwork"),
		LogLevel:                "info",
		SyncMaxParallel:         8,
		SyncRetries:             3,
		SyncRetryBackoff:        2 * time.Second,
		StorageWarnPercent:      80,
		StorageBlockSyncPercent: 90,
		SessionSecret:           "dev-insecure-secret",
		DramahitsBaseURL:        testDramahitsBaseURL,
		ReadHeaderTimeout:       10 * time.Second,
		IdleTimeout:             120 * time.Second,
		ShutdownTimeout:         15 * time.Second,
	}

	if cfg != want {
		t.Errorf("config default tidak sesuai\n didapat: %+v\n   ingin: %+v", cfg, want)
	}
}

// TestDramahitsBaseURLRequired memastikan URL provider tidak punya default
// dan harus diisi lewat environment.
func TestDramahitsBaseURLRequired(t *testing.T) {
	t.Parallel()

	_, err := LoadFrom(func(string) string { return "" })
	if err == nil {
		t.Fatal("LoadFrom tanpa VODLOCAL_DRAMAHITS_BASE_URL seharusnya error")
	}
	if msg := err.Error(); !strings.Contains(msg, envDramahitsBaseURL) {
		t.Errorf("pesan error %q tidak menyebut %q", msg, envDramahitsBaseURL)
	}
}

// TestLoadFromOverride memastikan setiap tipe (string, int, duration) bisa
// di-override lewat environment.
func TestLoadFromOverride(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"VODLOCAL_HTTP_ADDR":                  "127.0.0.1:9090",
		"VODLOCAL_DB_PATH":                    "custom/app.db",
		"VODLOCAL_MEDIA_ROOT":                 "/srv/media",
		"VODLOCAL_ARTWORK_ROOT":               "/srv/media/art",
		"VODLOCAL_LOG_LEVEL":                  "debug",
		"VODLOCAL_SYNC_MAX_PARALLEL":          "16",
		"VODLOCAL_SYNC_RETRIES":               "7",
		"VODLOCAL_SYNC_RETRY_BACKOFF":         "500ms",
		"VODLOCAL_STORAGE_WARN_PERCENT":       "70",
		"VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT": "85",
		"VODLOCAL_SESSION_SECRET":             "0123456789abcdef0123456789abcdef",
		"VODLOCAL_READ_HEADER_TIMEOUT":        "3s",
		"VODLOCAL_IDLE_TIMEOUT":               "45s",
		"VODLOCAL_SHUTDOWN_TIMEOUT":           "1m",
	}

	cfg, err := LoadFrom(mapEnv(env))
	if err != nil {
		t.Fatalf("LoadFrom(override) error tak terduga: %v", err)
	}

	want := Config{
		HTTPAddr:                "127.0.0.1:9090",
		DBPath:                  "custom/app.db",
		MediaRoot:               absDefault(t, "/srv/media"),
		ArtworkRoot:             absDefault(t, "/srv/media/art"),
		LogLevel:                "debug",
		SyncMaxParallel:         16,
		SyncRetries:             7,
		SyncRetryBackoff:        500 * time.Millisecond,
		StorageWarnPercent:      70,
		StorageBlockSyncPercent: 85,
		SessionSecret:           "0123456789abcdef0123456789abcdef",
		DramahitsBaseURL:        testDramahitsBaseURL,
		ReadHeaderTimeout:       3 * time.Second,
		IdleTimeout:             45 * time.Second,
		ShutdownTimeout:         60 * time.Second,
	}

	if cfg != want {
		t.Errorf("config hasil override tidak sesuai\n didapat: %+v\n   ingin: %+v", cfg, want)
	}
}

// TestLoadFromInvalid memastikan nilai tidak valid menghasilkan error yang
// menyebut nama environment variable terkait.
func TestLoadFromInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		env    map[string]string
		wantIn []string
	}{
		{
			name:   "sync max parallel nol",
			env:    map[string]string{"VODLOCAL_SYNC_MAX_PARALLEL": "0"},
			wantIn: []string{"VODLOCAL_SYNC_MAX_PARALLEL"},
		},
		{
			name:   "sync max parallel terlalu besar",
			env:    map[string]string{"VODLOCAL_SYNC_MAX_PARALLEL": "99"},
			wantIn: []string{"VODLOCAL_SYNC_MAX_PARALLEL"},
		},
		{
			name:   "sync max parallel bukan angka",
			env:    map[string]string{"VODLOCAL_SYNC_MAX_PARALLEL": "banyak"},
			wantIn: []string{"VODLOCAL_SYNC_MAX_PARALLEL"},
		},
		{
			name:   "sync retries terlalu besar",
			env:    map[string]string{"VODLOCAL_SYNC_RETRIES": "11"},
			wantIn: []string{"VODLOCAL_SYNC_RETRIES"},
		},
		{
			name:   "dramahits base url tidak valid",
			env:    map[string]string{"VODLOCAL_DRAMAHITS_BASE_URL": "bukan-url"},
			wantIn: []string{"VODLOCAL_DRAMAHITS_BASE_URL"},
		},
		{
			name:   "log level tidak dikenal",
			env:    map[string]string{"VODLOCAL_LOG_LEVEL": "verbose"},
			wantIn: []string{"VODLOCAL_LOG_LEVEL", "verbose"},
		},
		{
			name:   "durasi bukan durasi",
			env:    map[string]string{"VODLOCAL_READ_HEADER_TIMEOUT": "abc"},
			wantIn: []string{"VODLOCAL_READ_HEADER_TIMEOUT"},
		},
		{
			name:   "durasi nol",
			env:    map[string]string{"VODLOCAL_IDLE_TIMEOUT": "0s"},
			wantIn: []string{"VODLOCAL_IDLE_TIMEOUT"},
		},
		{
			name:   "durasi negatif",
			env:    map[string]string{"VODLOCAL_SHUTDOWN_TIMEOUT": "-5s"},
			wantIn: []string{"VODLOCAL_SHUTDOWN_TIMEOUT"},
		},
		{
			name:   "retry backoff tidak valid",
			env:    map[string]string{"VODLOCAL_SYNC_RETRY_BACKOFF": "2 detik"},
			wantIn: []string{"VODLOCAL_SYNC_RETRY_BACKOFF"},
		},
		{
			name:   "storage warn percent nol",
			env:    map[string]string{"VODLOCAL_STORAGE_WARN_PERCENT": "0"},
			wantIn: []string{"VODLOCAL_STORAGE_WARN_PERCENT"},
		},
		{
			name:   "storage block percent di atas seratus",
			env:    map[string]string{"VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT": "101"},
			wantIn: []string{"VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT"},
		},
		{
			name:   "session secret terlalu pendek",
			env:    map[string]string{"VODLOCAL_SESSION_SECRET": "pendek"},
			wantIn: []string{"VODLOCAL_SESSION_SECRET", "16"},
		},
		{
			name: "beberapa nilai salah sekaligus",
			env: map[string]string{
				"VODLOCAL_SYNC_MAX_PARALLEL": "0",
				"VODLOCAL_LOG_LEVEL":         "verbose",
				"VODLOCAL_IDLE_TIMEOUT":      "abc",
			},
			wantIn: []string{"VODLOCAL_SYNC_MAX_PARALLEL", "VODLOCAL_LOG_LEVEL", "VODLOCAL_IDLE_TIMEOUT"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := LoadFrom(mapEnv(tc.env))
			if err == nil {
				t.Fatalf("LoadFrom(%v) seharusnya error, didapat config %+v", tc.env, cfg)
			}
			msg := err.Error()
			for _, want := range tc.wantIn {
				if !strings.Contains(msg, want) {
					t.Errorf("pesan error %q tidak menyebut %q", msg, want)
				}
			}
			if cfg != (Config{}) {
				t.Errorf("config harus zero value saat error, didapat %+v", cfg)
			}
		})
	}
}

// TestLoadFromStorageThreshold memastikan warn harus lebih kecil dari block.
func TestLoadFromStorageThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		warn  string
		block string
	}{
		{name: "sama besar", warn: "90", block: "90"},
		{name: "warn lebih besar", warn: "95", block: "90"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadFrom(mapEnv(map[string]string{
				"VODLOCAL_STORAGE_WARN_PERCENT":       tc.warn,
				"VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT": tc.block,
			}))
			if err == nil {
				t.Fatalf("warn=%s block=%s seharusnya error", tc.warn, tc.block)
			}
			msg := err.Error()
			for _, want := range []string{"VODLOCAL_STORAGE_WARN_PERCENT", "VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT"} {
				if !strings.Contains(msg, want) {
					t.Errorf("pesan error %q tidak menyebut %q", msg, want)
				}
			}
		})
	}

	// warn < block harus lolos.
	if _, err := LoadFrom(mapEnv(map[string]string{
		"VODLOCAL_STORAGE_WARN_PERCENT":       "50",
		"VODLOCAL_STORAGE_BLOCK_SYNC_PERCENT": "60",
	})); err != nil {
		t.Errorf("warn < block seharusnya valid, didapat error: %v", err)
	}
}

// TestLoadFromResolvesRelativePaths memastikan MediaRoot dan ArtworkRoot
// di-resolve menjadi absolut.
func TestLoadFromResolvesRelativePaths(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(mapEnv(map[string]string{
		"VODLOCAL_MEDIA_ROOT":   "media",
		"VODLOCAL_ARTWORK_ROOT": "./media/artwork",
	}))
	if err != nil {
		t.Fatalf("LoadFrom(relative path) error tak terduga: %v", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "media root", got: cfg.MediaRoot, want: absDefault(t, "media")},
		{name: "artwork root", got: cfg.ArtworkRoot, want: absDefault(t, filepath.Join("media", "artwork"))},
	}
	for _, tc := range tests {
		if !filepath.IsAbs(tc.got) {
			t.Errorf("%s: %q bukan path absolut", tc.name, tc.got)
		}
		if tc.got != filepath.Clean(tc.want) {
			t.Errorf("%s: didapat %q, ingin %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestLoadFromEmptyValuesUseDefault memastikan env kosong dianggap tidak diset.
func TestLoadFromEmptyValuesUseDefault(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(mapEnv(map[string]string{
		"VODLOCAL_HTTP_ADDR":         "   ",
		"VODLOCAL_LOG_LEVEL":         "",
		"VODLOCAL_SYNC_MAX_PARALLEL": "",
		"VODLOCAL_IDLE_TIMEOUT":      "",
	}))
	if err != nil {
		t.Fatalf("LoadFrom(env kosong) error tak terduga: %v", err)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, ingin %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("LogLevel = %q, ingin %q", cfg.LogLevel, defaultLogLevel)
	}
	if cfg.SyncMaxParallel != defaultSyncMaxParallel {
		t.Errorf("SyncMaxParallel = %d, ingin %d", cfg.SyncMaxParallel, defaultSyncMaxParallel)
	}
	if cfg.IdleTimeout != defaultIdleTimeout {
		t.Errorf("IdleTimeout = %s, ingin %s", cfg.IdleTimeout, defaultIdleTimeout)
	}
}

// TestLoad memastikan Load membaca environment process dan LoadFrom(nil)
// berperilaku sama.
func TestLoad(t *testing.T) {
	t.Setenv("VODLOCAL_HTTP_ADDR", "127.0.0.1:9999")
	t.Setenv("VODLOCAL_LOG_LEVEL", "WARN")
	t.Setenv("VODLOCAL_SYNC_RETRIES", "5")
	t.Setenv("VODLOCAL_DRAMAHITS_BASE_URL", testDramahitsBaseURL)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error tak terduga: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9999" {
		t.Errorf("HTTPAddr = %q, ingin %q", cfg.HTTPAddr, "127.0.0.1:9999")
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, ingin %q (harus lowercase)", cfg.LogLevel, "warn")
	}
	if cfg.SyncRetries != 5 {
		t.Errorf("SyncRetries = %d, ingin 5", cfg.SyncRetries)
	}

	nilCfg, err := LoadFrom(nil)
	if err != nil {
		t.Fatalf("LoadFrom(nil) error tak terduga: %v", err)
	}
	if nilCfg != cfg {
		t.Errorf("LoadFrom(nil) = %+v, ingin sama dengan Load() = %+v", nilCfg, cfg)
	}
}

// TestValidateEmptyPaths memastikan path wajib tidak boleh kosong. Kasus ini
// tidak bisa muncul lewat environment (env kosong berarti default), jadi
// validate dipanggil langsung.
func TestValidateEmptyPaths(t *testing.T) {
	t.Parallel()

	cfg := Config{
		HTTPAddr:                defaultHTTPAddr,
		DBPath:                  "",
		MediaRoot:               "",
		ArtworkRoot:             "",
		LogLevel:                defaultLogLevel,
		SyncMaxParallel:         defaultSyncMaxParallel,
		SyncRetries:             defaultSyncRetries,
		SyncRetryBackoff:        defaultSyncRetryBackoff,
		StorageWarnPercent:      defaultStorageWarnPercent,
		StorageBlockSyncPercent: defaultStorageBlockSyncPercent,
		SessionSecret:           defaultSessionSecret,
		ReadHeaderTimeout:       defaultReadHeaderTimeout,
		IdleTimeout:             defaultIdleTimeout,
		ShutdownTimeout:         defaultShutdownTimeout,
	}

	err := validate(cfg)
	if err == nil {
		t.Fatal("validate(config dengan path kosong) seharusnya error")
	}
	msg := err.Error()
	for _, want := range []string{"VODLOCAL_DB_PATH", "VODLOCAL_MEDIA_ROOT", "VODLOCAL_ARTWORK_ROOT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("pesan error %q tidak menyebut %q", msg, want)
		}
	}

	// Config yang sudah benar tidak boleh menghasilkan error.
	cfg.DBPath = defaultDBPath
	cfg.MediaRoot = absDefault(t, defaultMediaRoot)
	cfg.ArtworkRoot = absDefault(t, defaultArtworkRoot)
	cfg.DramahitsBaseURL = testDramahitsBaseURL
	if err := validate(cfg); err != nil {
		t.Errorf("validate(config valid) error tak terduga: %v", err)
	}
}

// TestParseInt menguji helper parseInt secara langsung.
func TestParseInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		def     int
		want    int
		wantErr bool
	}{
		{name: "tidak diset", raw: "", def: 8, want: 8},
		{name: "spasi saja", raw: "   ", def: 8, want: 8},
		{name: "valid", raw: "12", def: 8, want: 12},
		{name: "valid dengan spasi", raw: " 12 ", def: 8, want: 12},
		{name: "nol", raw: "0", def: 8, want: 0},
		{name: "negatif", raw: "-3", def: 8, want: -3},
		{name: "bukan angka", raw: "abc", def: 8, want: 8, wantErr: true},
		{name: "desimal", raw: "1.5", def: 8, want: 8, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseInt(mapEnv(map[string]string{"VODLOCAL_UNDER_TEST": tc.raw}), "VODLOCAL_UNDER_TEST", tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseInt(%q) seharusnya error", tc.raw)
				}
				if !strings.Contains(err.Error(), "VODLOCAL_UNDER_TEST") {
					t.Errorf("pesan error %q tidak menyebut nama env", err.Error())
				}
			} else if err != nil {
				t.Fatalf("parseInt(%q) error tak terduga: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseInt(%q) = %d, ingin %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseDuration menguji helper parseDuration secara langsung.
func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		def     time.Duration
		want    time.Duration
		wantErr bool
	}{
		{name: "tidak diset", raw: "", def: 2 * time.Second, want: 2 * time.Second},
		{name: "detik", raw: "30s", def: time.Second, want: 30 * time.Second},
		{name: "milidetik", raw: "250ms", def: time.Second, want: 250 * time.Millisecond},
		{name: "menit", raw: "2m", def: time.Second, want: 2 * time.Minute},
		{name: "gabungan", raw: "1m30s", def: time.Second, want: 90 * time.Second},
		{name: "tanpa satuan", raw: "10", def: time.Second, want: time.Second, wantErr: true},
		{name: "bukan durasi", raw: "abc", def: time.Second, want: time.Second, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDuration(mapEnv(map[string]string{"VODLOCAL_UNDER_TEST": tc.raw}), "VODLOCAL_UNDER_TEST", tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDuration(%q) seharusnya error", tc.raw)
				}
				if !strings.Contains(err.Error(), "VODLOCAL_UNDER_TEST") {
					t.Errorf("pesan error %q tidak menyebut nama env", err.Error())
				}
			} else if err != nil {
				t.Fatalf("parseDuration(%q) error tak terduga: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseDuration(%q) = %s, ingin %s", tc.raw, got, tc.want)
			}
		})
	}
}

// countLeaves menghitung jumlah error daun pada pohon error (hasil
// errors.Join bisa bersarang).
func countLeaves(err error) int {
	if err == nil {
		return 0
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		n := 0
		for _, child := range multi.Unwrap() {
			n += countLeaves(child)
		}
		if n > 0 {
			return n
		}
	}
	return 1
}

// TestErrorsJoin memastikan semua pelanggaran dilaporkan sekaligus.
func TestErrorsJoin(t *testing.T) {
	t.Parallel()

	_, err := LoadFrom(mapEnv(map[string]string{
		"VODLOCAL_SYNC_MAX_PARALLEL":    "0",
		"VODLOCAL_STORAGE_WARN_PERCENT": "99",
		"VODLOCAL_SESSION_SECRET":       "x",
	}))
	if err == nil {
		t.Fatal("seharusnya error")
	}

	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("error seharusnya hasil errors.Join, didapat %T", err)
	}
	if n := countLeaves(err); n < 3 {
		t.Errorf("jumlah error tergabung = %d, ingin >= 3 (pesan: %q)", n, err.Error())
	}
}

// TestAdminCredentials memastikan user dan password admin harus diisi bersama.
func TestAdminCredentials(t *testing.T) {
	t.Parallel()

	t.Run("keduanya kosong", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(mapEnv(map[string]string{}))
		if err != nil {
			t.Fatalf("error tak terduga: %v", err)
		}
		if cfg.AdminUser != "" || cfg.AdminPassword != "" {
			t.Fatalf("admin harus kosong, didapat %q/%q", cfg.AdminUser, cfg.AdminPassword)
		}
	})

	t.Run("keduanya diisi", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_ADMIN_USER":     " operator ",
			"VODLOCAL_ADMIN_PASSWORD": "rahasia-panjang",
		}))
		if err != nil {
			t.Fatalf("error tak terduga: %v", err)
		}
		if cfg.AdminUser != "operator" {
			t.Errorf("AdminUser = %q, ingin %q", cfg.AdminUser, "operator")
		}
		if cfg.AdminPassword != "rahasia-panjang" {
			t.Errorf("AdminPassword tidak sesuai")
		}
	})

	t.Run("hanya user", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFrom(mapEnv(map[string]string{"VODLOCAL_ADMIN_USER": "operator"}))
		if err == nil || !strings.Contains(err.Error(), "VODLOCAL_ADMIN_PASSWORD") {
			t.Fatalf("seharusnya error menyebut VODLOCAL_ADMIN_PASSWORD, didapat %v", err)
		}
	})

	t.Run("hanya password", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFrom(mapEnv(map[string]string{"VODLOCAL_ADMIN_PASSWORD": "rahasia-panjang"}))
		if err == nil || !strings.Contains(err.Error(), "VODLOCAL_ADMIN_USER") {
			t.Fatalf("seharusnya error menyebut VODLOCAL_ADMIN_USER, didapat %v", err)
		}
	})

	t.Run("password terlalu pendek", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_ADMIN_USER":     "operator",
			"VODLOCAL_ADMIN_PASSWORD": "pendek",
		}))
		if err == nil || !strings.Contains(err.Error(), "VODLOCAL_ADMIN_PASSWORD") {
			t.Fatalf("seharusnya error password pendek, didapat %v", err)
		}
	})
}

// TestNotificationConfig memastikan konfigurasi webhook dan Telegram valid.
func TestNotificationConfig(t *testing.T) {
	t.Parallel()

	t.Run("webhook valid", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_NOTIFY_WEBHOOK_URL": "https://example.com/hook",
		}))
		if err != nil {
			t.Fatalf("error tak terduga: %v", err)
		}
		if cfg.NotifyWebhookURL != "https://example.com/hook" {
			t.Errorf("URL = %q", cfg.NotifyWebhookURL)
		}
	})

	t.Run("webhook tidak valid", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_NOTIFY_WEBHOOK_URL": "ftp://example.com",
		}))
		if err == nil || !strings.Contains(err.Error(), "VODLOCAL_NOTIFY_WEBHOOK_URL") {
			t.Fatalf("seharusnya error URL, didapat %v", err)
		}
	})

	t.Run("telegram harus berpasangan", func(t *testing.T) {
		t.Parallel()
		_, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_TELEGRAM_TOKEN": "123:abc",
		}))
		if err == nil || !strings.Contains(err.Error(), "VODLOCAL_TELEGRAM_CHAT_ID") {
			t.Fatalf("seharusnya error chat id, didapat %v", err)
		}
	})

	t.Run("telegram lengkap", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(mapEnv(map[string]string{
			"VODLOCAL_TELEGRAM_TOKEN":   "123:abc",
			"VODLOCAL_TELEGRAM_CHAT_ID": "-100123",
		}))
		if err != nil {
			t.Fatalf("error tak terduga: %v", err)
		}
		if cfg.TelegramChatID != "-100123" {
			t.Errorf("chat id = %q", cfg.TelegramChatID)
		}
	})
}
