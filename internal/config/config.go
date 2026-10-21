// Package config memuat konfigurasi aplikasi dari environment variable.
//
// Semua variabel memakai prefix "VODLOCAL_", misalnya VODLOCAL_HTTP_ADDR. Setiap
// nilai yang tidak diset (atau berisi string kosong) akan memakai default yang
// didefinisikan di paket ini, kecuali VODLOCAL_DRAMAHITS_BASE_URL yang wajib
// diisi karena berbeda untuk setiap environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// envPrefix adalah prefix wajib untuk seluruh environment variable.
const envPrefix = "VODLOCAL_"

// Nama environment variable yang dikenali.
const (
	envHTTPAddr                = envPrefix + "HTTP_ADDR"
	envDBPath                  = envPrefix + "DB_PATH"
	envMediaRoot               = envPrefix + "MEDIA_ROOT"
	envArtworkRoot             = envPrefix + "ARTWORK_ROOT"
	envLogLevel                = envPrefix + "LOG_LEVEL"
	envSyncMaxParallel         = envPrefix + "SYNC_MAX_PARALLEL"
	envSyncRetries             = envPrefix + "SYNC_RETRIES"
	envSyncRetryBackoff        = envPrefix + "SYNC_RETRY_BACKOFF"
	envStorageWarnPercent      = envPrefix + "STORAGE_WARN_PERCENT"
	envStorageBlockSyncPercent = envPrefix + "STORAGE_BLOCK_SYNC_PERCENT"
	envSessionSecret           = envPrefix + "SESSION_SECRET"
	envAdminUser               = envPrefix + "ADMIN_USER"
	envAdminPassword           = envPrefix + "ADMIN_PASSWORD"
	envNotifyWebhookURL        = envPrefix + "NOTIFY_WEBHOOK_URL"
	envTelegramToken           = envPrefix + "TELEGRAM_TOKEN"
	envTelegramChatID          = envPrefix + "TELEGRAM_CHAT_ID"
	envDramahitsBaseURL        = envPrefix + "DRAMAHITS_BASE_URL"
	envDensgoBaseURL           = envPrefix + "DENSGO_BASE_URL"
	envIngestMaxEpisodes       = envPrefix + "INGEST_MAX_EPISODES"
	envReadHeaderTimeout       = envPrefix + "READ_HEADER_TIMEOUT"
	envIdleTimeout             = envPrefix + "IDLE_TIMEOUT"
	envShutdownTimeout         = envPrefix + "SHUTDOWN_TIMEOUT"
)

// Nilai default untuk setiap field Config.
const (
	defaultHTTPAddr                = ":8080"
	defaultDBPath                  = "data/app.db"
	defaultMediaRoot               = "data/media"
	defaultArtworkRoot             = "data/media/artwork"
	defaultLogLevel                = "info"
	defaultSyncMaxParallel         = 8
	defaultSyncRetries             = 3
	defaultSyncRetryBackoff        = 2 * time.Second
	defaultStorageWarnPercent      = 80
	defaultStorageBlockSyncPercent = 90
	defaultSessionSecret           = "dev-insecure-secret"
	defaultDensgoBaseURL           = "https://pp.dens.tv:777/index.php/v3070/adps_list_movies_v1"
	defaultIngestMaxEpisodes       = 0
	defaultReadHeaderTimeout       = 10 * time.Second
	defaultIdleTimeout             = 120 * time.Second
	defaultShutdownTimeout         = 15 * time.Second
)

// Batas nilai yang dianggap valid.
const (
	minSyncMaxParallel     = 1
	maxSyncMaxParallel     = 64
	minSyncRetries         = 0
	maxSyncRetries         = 10
	minStoragePercent      = 1
	maxStoragePercent      = 100
	minSessionSecretLength = 16
	minAdminPasswordLength = 8
	logLevelDebug          = "debug"
	logLevelInfo           = "info"
	logLevelWarn           = "warn"
	logLevelError          = "error"
)

// Config adalah seluruh konfigurasi runtime aplikasi.
type Config struct {
	HTTPAddr                string
	DBPath                  string
	MediaRoot               string
	ArtworkRoot             string
	LogLevel                string
	SyncMaxParallel         int
	SyncRetries             int
	SyncRetryBackoff        time.Duration
	StorageWarnPercent      int
	StorageBlockSyncPercent int
	SessionSecret           string
	AdminUser               string
	AdminPassword           string
	NotifyWebhookURL        string
	TelegramToken           string
	TelegramChatID          string
	DramahitsBaseURL        string
	DensgoBaseURL           string
	IngestMaxEpisodes       int
	ReadHeaderTimeout       time.Duration
	IdleTimeout             time.Duration
	ShutdownTimeout         time.Duration
}

// Load memuat konfigurasi dari environment process (os.Getenv).
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom memuat konfigurasi memakai fungsi getenv yang diberikan. Semua
// pelanggaran validasi dikumpulkan lalu dikembalikan sebagai satu error, dan
// pesannya selalu menyebut nama environment variable yang bermasalah.
// Bila getenv nil, os.Getenv yang dipakai.
func LoadFrom(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	var errs []error

	cfg := Config{
		HTTPAddr:                readString(getenv, envHTTPAddr, defaultHTTPAddr),
		DBPath:                  readString(getenv, envDBPath, defaultDBPath),
		MediaRoot:               readString(getenv, envMediaRoot, defaultMediaRoot),
		ArtworkRoot:             readString(getenv, envArtworkRoot, defaultArtworkRoot),
		LogLevel:                strings.ToLower(readString(getenv, envLogLevel, defaultLogLevel)),
		SyncRetryBackoff:        mustDuration(&errs, getenv, envSyncRetryBackoff, defaultSyncRetryBackoff),
		SessionSecret:           readString(getenv, envSessionSecret, defaultSessionSecret),
		AdminUser:               strings.TrimSpace(readString(getenv, envAdminUser, "")),
		AdminPassword:           readString(getenv, envAdminPassword, ""),
		NotifyWebhookURL:        strings.TrimSpace(readString(getenv, envNotifyWebhookURL, "")),
		TelegramToken:           strings.TrimSpace(readString(getenv, envTelegramToken, "")),
		TelegramChatID:          strings.TrimSpace(readString(getenv, envTelegramChatID, "")),
		DramahitsBaseURL:        strings.TrimSpace(readString(getenv, envDramahitsBaseURL, "")),
		DensgoBaseURL:           strings.TrimSpace(readString(getenv, envDensgoBaseURL, defaultDensgoBaseURL)),
		IngestMaxEpisodes:       mustInt(&errs, getenv, envIngestMaxEpisodes, defaultIngestMaxEpisodes),
		ReadHeaderTimeout:       mustDuration(&errs, getenv, envReadHeaderTimeout, defaultReadHeaderTimeout),
		IdleTimeout:             mustDuration(&errs, getenv, envIdleTimeout, defaultIdleTimeout),
		ShutdownTimeout:         mustDuration(&errs, getenv, envShutdownTimeout, defaultShutdownTimeout),
		SyncMaxParallel:         mustInt(&errs, getenv, envSyncMaxParallel, defaultSyncMaxParallel),
		SyncRetries:             mustInt(&errs, getenv, envSyncRetries, defaultSyncRetries),
		StorageWarnPercent:      mustInt(&errs, getenv, envStorageWarnPercent, defaultStorageWarnPercent),
		StorageBlockSyncPercent: mustInt(&errs, getenv, envStorageBlockSyncPercent, defaultStorageBlockSyncPercent),
	}

	// MediaRoot dan ArtworkRoot dipakai untuk akses filesystem, jadi harus
	// absolut agar tidak bergantung pada working directory process.
	if cfg.MediaRoot != "" {
		abs, err := filepath.Abs(cfg.MediaRoot)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: gagal resolve path %q: %w", envMediaRoot, cfg.MediaRoot, err))
		} else {
			cfg.MediaRoot = abs
		}
	}
	if cfg.ArtworkRoot != "" {
		abs, err := filepath.Abs(cfg.ArtworkRoot)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: gagal resolve path %q: %w", envArtworkRoot, cfg.ArtworkRoot, err))
		} else {
			cfg.ArtworkRoot = abs
		}
	}

	if err := validate(cfg); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// validate memeriksa seluruh nilai Config dan mengumpulkan semua pelanggaran.
func validate(cfg Config) error {
	var errs []error

	if cfg.DBPath == "" {
		errs = append(errs, fmt.Errorf("%s: tidak boleh kosong", envDBPath))
	}
	if cfg.MediaRoot == "" {
		errs = append(errs, fmt.Errorf("%s: tidak boleh kosong", envMediaRoot))
	}
	if cfg.ArtworkRoot == "" {
		errs = append(errs, fmt.Errorf("%s: tidak boleh kosong", envArtworkRoot))
	}

	switch cfg.LogLevel {
	case logLevelDebug, logLevelInfo, logLevelWarn, logLevelError:
	default:
		errs = append(errs, fmt.Errorf("%s: nilai %q tidak dikenal, pilih salah satu: debug, info, warn, error", envLogLevel, cfg.LogLevel))
	}

	if v := cfg.SyncMaxParallel; v < minSyncMaxParallel || v > maxSyncMaxParallel {
		errs = append(errs, fmt.Errorf("%s: harus di antara %d dan %d, didapat %d", envSyncMaxParallel, minSyncMaxParallel, maxSyncMaxParallel, v))
	}
	if v := cfg.SyncRetries; v < minSyncRetries || v > maxSyncRetries {
		errs = append(errs, fmt.Errorf("%s: harus di antara %d dan %d, didapat %d", envSyncRetries, minSyncRetries, maxSyncRetries, v))
	}

	warn, block := cfg.StorageWarnPercent, cfg.StorageBlockSyncPercent
	warnValid := warn >= minStoragePercent && warn <= maxStoragePercent
	blockValid := block >= minStoragePercent && block <= maxStoragePercent
	if !warnValid {
		errs = append(errs, fmt.Errorf("%s: harus di antara %d dan %d, didapat %d", envStorageWarnPercent, minStoragePercent, maxStoragePercent, warn))
	}
	if !blockValid {
		errs = append(errs, fmt.Errorf("%s: harus di antara %d dan %d, didapat %d", envStorageBlockSyncPercent, minStoragePercent, maxStoragePercent, block))
	}
	if warnValid && blockValid && warn >= block {
		errs = append(errs, fmt.Errorf("%s (%d) harus lebih kecil dari %s (%d)", envStorageWarnPercent, warn, envStorageBlockSyncPercent, block))
	}

	durations := []struct {
		name  string
		value time.Duration
	}{
		{envSyncRetryBackoff, cfg.SyncRetryBackoff},
		{envReadHeaderTimeout, cfg.ReadHeaderTimeout},
		{envIdleTimeout, cfg.IdleTimeout},
		{envShutdownTimeout, cfg.ShutdownTimeout},
	}
	for _, d := range durations {
		if d.value <= 0 {
			errs = append(errs, fmt.Errorf("%s: harus durasi > 0, didapat %q", d.name, d.value.String()))
		}
	}

	if n := len(cfg.SessionSecret); n < minSessionSecretLength {
		errs = append(errs, fmt.Errorf("%s: minimal %d karakter, didapat %d", envSessionSecret, minSessionSecretLength, n))
	}

	hasUser := cfg.AdminUser != ""
	hasPassword := cfg.AdminPassword != ""
	if hasUser != hasPassword {
		errs = append(errs, fmt.Errorf("%s dan %s harus diisi bersamaan atau dikosongkan keduanya", envAdminUser, envAdminPassword))
	}
	if hasPassword && len(cfg.AdminPassword) < minAdminPasswordLength {
		errs = append(errs, fmt.Errorf("%s: minimal %d karakter", envAdminPassword, minAdminPasswordLength))
	}

	if cfg.NotifyWebhookURL != "" {
		u, err := url.Parse(cfg.NotifyWebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: harus URL http/https yang valid", envNotifyWebhookURL))
		}
	}
	hasTelegramToken := cfg.TelegramToken != ""
	hasTelegramChat := cfg.TelegramChatID != ""
	if hasTelegramToken != hasTelegramChat {
		errs = append(errs, fmt.Errorf("%s dan %s harus diisi bersamaan atau dikosongkan keduanya", envTelegramToken, envTelegramChatID))
	}

	if cfg.DramahitsBaseURL == "" {
		errs = append(errs, fmt.Errorf("%s: wajib diisi", envDramahitsBaseURL))
	} else if u, err := url.Parse(cfg.DramahitsBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s: harus URL http/https yang valid", envDramahitsBaseURL))
	}
	if u, err := url.Parse(cfg.DensgoBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s: harus URL http/https yang valid", envDensgoBaseURL))
	}
	if cfg.IngestMaxEpisodes < 0 {
		errs = append(errs, fmt.Errorf("%s: tidak boleh negatif, didapat %d", envIngestMaxEpisodes, cfg.IngestMaxEpisodes))
	}

	return errors.Join(errs...)
}

// readString mengembalikan nilai envName bila terisi, atau def bila tidak.
func readString(getenv func(string) string, envName, def string) string {
	if v := strings.TrimSpace(getenv(envName)); v != "" {
		return v
	}
	return def
}

// parseInt membaca envName sebagai int. Bila tidak diset hasilnya def, dan
// bila gagal di-parse hasilnya def disertai error.
func parseInt(getenv func(string) string, envName string, def int) (int, error) {
	raw := strings.TrimSpace(getenv(envName))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s: nilai %q bukan angka bulat yang valid", envName, raw)
	}
	return v, nil
}

// parseDuration membaca envName sebagai time.Duration (mis. "10s", "2m").
// Bila tidak diset hasilnya def, dan bila gagal di-parse hasilnya def disertai
// error.
func parseDuration(getenv func(string) string, envName string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(envName))
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s: durasi %q tidak valid (contoh: 10s, 2m, 500ms)", envName, raw)
	}
	return v, nil
}

// mustInt adalah parseInt yang mencatat error ke errs dan selalu mengembalikan
// nilai default saat gagal, agar validasi berikutnya tidak menghasilkan error
// berantai yang membingungkan.
func mustInt(errs *[]error, getenv func(string) string, envName string, def int) int {
	v, err := parseInt(getenv, envName, def)
	if err != nil {
		*errs = append(*errs, err)
	}
	return v
}

// mustDuration adalah parseDuration yang mencatat error ke errs.
func mustDuration(errs *[]error, getenv func(string) string, envName string, def time.Duration) time.Duration {
	v, err := parseDuration(getenv, envName, def)
	if err != nil {
		*errs = append(*errs, err)
	}
	return v
}
