package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"vodlocal/internal/appsettings"
)

// Kunci app_settings yang dikelola panel (§10.6).
const (
	settingMaxShortSide            = appsettings.MaxShortSideKey
	settingMaxParallelSegments     = "sync.max_parallel_segments"
	settingStorageWarnPercent      = "storage.warn_percent"
	settingStorageBlockSyncPercent = "storage.block_sync_percent"
	settingProgressDebounceSeconds = "playback.progress_debounce_seconds"
)

// Jenis field pada formulir pengaturan.
const (
	settingKindInt    = "int"
	settingKindBool   = "bool"
	settingKindText   = "text"
	settingKindSelect = "select"
)

// settingOption adalah satu pilihan pada settingKindSelect.
type settingOption struct {
	Value string
	Label string
}

// settingDef mendeskripsikan satu field pengaturan.
type settingDef struct {
	Key     string
	Label   string
	Kind    string
	Default string
	Min     int
	Max     int
	Options []settingOption
}

// settingDefs adalah daftar field pengaturan dalam urutan tampil (§10.6).
var settingDefs = []settingDef{
	{
		Key:     settingMaxShortSide,
		Label:   "Batas sisi pendek varian video",
		Kind:    settingKindSelect,
		Default: "720",
		Options: []settingOption{
			{Value: "360", Label: "360p"},
			{Value: "480", Label: "480p"},
			{Value: "540", Label: "540p"},
			{Value: "720", Label: "720p"},
			{Value: "1080", Label: "1080p"},
		},
	},
	{Key: settingMaxParallelSegments, Label: "Segmen paralel maksimum", Kind: settingKindInt, Default: "8", Min: 1, Max: 64},
	{Key: settingStorageWarnPercent, Label: "Peringatan storage (%)", Kind: settingKindInt, Default: "80", Min: 1, Max: 100},
	{Key: settingStorageBlockSyncPercent, Label: "Blokir sync saat storage (%)", Kind: settingKindInt, Default: "90", Min: 1, Max: 100},
	{Key: settingProgressDebounceSeconds, Label: "Debounce simpan progress (detik)", Kind: settingKindInt, Default: "20", Min: 1, Max: 3600},
}

// settingField adalah satu baris formulir pengaturan.
type settingField struct {
	Key     string
	Label   string
	Kind    string
	Value   string
	Checked bool
	Error   string
	Options []settingOption
}

// settingsData adalah data halaman /settings.
type settingsData struct {
	base
	Fields []settingField
}

// defaultSetting mengembalikan nilai default untuk key.
func defaultSetting(key string) string {
	for _, def := range settingDefs {
		if def.Key == key {
			return def.Default
		}
	}
	return ""
}

// handleSettingsForm melayani GET /settings.
func (a *Admin) handleSettingsForm(w http.ResponseWriter, r *http.Request) {
	values, err := a.readSettings(r.Context())
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca pengaturan.")
		return
	}
	a.renderSettings(w, r, values, nil, http.StatusOK)
}

// renderSettings merender formulir dengan nilai dan error per field.
func (a *Admin) renderSettings(w http.ResponseWriter, r *http.Request, values map[string]string, fieldErrors map[string]string, status int) {
	fields := make([]settingField, 0, len(settingDefs))
	for _, def := range settingDefs {
		value := values[def.Key]
		if strings.TrimSpace(value) == "" {
			value = def.Default
		}
		fields = append(fields, settingField{
			Key:     def.Key,
			Label:   def.Label,
			Kind:    def.Kind,
			Value:   value,
			Checked: def.Kind == settingKindBool && isTruthy(value),
			Error:   fieldErrors[def.Key],
			Options: def.Options,
		})
	}

	a.renderPage(w, r, "settings", settingsData{
		base:   a.base(w, r, "Pengaturan", "settings"),
		Fields: fields,
	}, status)
}

// handleSettingsSave melayani POST /settings.
func (a *Admin) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderError(w, r, http.StatusBadRequest, "Formulir tidak dapat dibaca.")
		return
	}

	values := make(map[string]string, len(settingDefs))
	fieldErrors := make(map[string]string, len(settingDefs))

	for _, def := range settingDefs {
		switch def.Kind {
		case settingKindBool:
			if isTruthy(r.PostFormValue(def.Key)) {
				values[def.Key] = "true"
			} else {
				values[def.Key] = "false"
			}
		case settingKindInt:
			raw := strings.TrimSpace(r.PostFormValue(def.Key))
			number, err := strconv.Atoi(raw)
			switch {
			case raw == "":
				fieldErrors[def.Key] = "wajib diisi"
			case err != nil:
				fieldErrors[def.Key] = "harus berupa angka"
			case number < def.Min || number > def.Max:
				fieldErrors[def.Key] = fmt.Sprintf("harus di antara %d dan %d", def.Min, def.Max)
			default:
				values[def.Key] = strconv.Itoa(number)
			}
		case settingKindSelect:
			raw := strings.TrimSpace(r.PostFormValue(def.Key))
			if !validSettingOption(def, raw) {
				fieldErrors[def.Key] = "pilihan tidak valid"
			} else {
				values[def.Key] = raw
			}
		default:
			raw := strings.TrimSpace(r.PostFormValue(def.Key))
			switch {
			case raw == "":
				fieldErrors[def.Key] = "wajib diisi"
			case len([]rune(raw)) > maxQueryLength:
				fieldErrors[def.Key] = fmt.Sprintf("maksimal %d karakter", maxQueryLength)
			default:
				values[def.Key] = raw
			}
		}
	}

	// Aturan silang: ambang blokir harus lebih tinggi dari ambang peringatan.
	if _, bad := fieldErrors[settingStorageWarnPercent]; !bad {
		if _, bad := fieldErrors[settingStorageBlockSyncPercent]; !bad {
			warn, _ := strconv.Atoi(values[settingStorageWarnPercent])
			block, _ := strconv.Atoi(values[settingStorageBlockSyncPercent])
			if warn >= block {
				fieldErrors[settingStorageBlockSyncPercent] = fmt.Sprintf(
					"harus lebih besar dari %s (%d)", settingStorageWarnPercent, warn)
			}
		}
	}

	if len(fieldErrors) > 0 {
		a.renderSettings(w, r, values, fieldErrors, http.StatusUnprocessableEntity)
		return
	}

	if err := a.saveSettings(r.Context(), values); err != nil {
		a.internalError(w, r, err, "Gagal menyimpan pengaturan.")
		return
	}

	a.log.Info("admin: pengaturan disimpan", "username", a.sessionUsername(r))
	a.flashTo(r, "success", "Pengaturan disimpan.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// readSettings membaca app_settings sebagai peta key→value.
func (a *Admin) readSettings(ctx context.Context) (map[string]string, error) {
	rows, err := a.read.QueryContext(ctx, `SELECT key, value FROM app_settings`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	values := make(map[string]string, len(settingDefs))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

// saveSettings melakukan upsert seluruh nilai dalam satu transaksi.
func (a *Admin) saveSettings(ctx context.Context, values map[string]string) error {
	tx, err := a.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stamp := a.stamp()
	for _, def := range settingDefs {
		value, ok := values[def.Key]
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			def.Key, value, stamp,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// validSettingOption melaporkan apakah value termasuk pilihan settingDef.
func validSettingOption(def settingDef, value string) bool {
	for _, option := range def.Options {
		if option.Value == value {
			return true
		}
	}
	return false
}

// settingInt membaca satu pengaturan sebagai int, memakai default bila kosong
// atau tidak valid.
func (a *Admin) settingInt(ctx context.Context, key string) int {
	fallback, _ := strconv.Atoi(defaultSetting(key))

	var raw string
	err := a.read.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fallback
	case err != nil:
		a.log.Warn("admin: gagal membaca pengaturan", "key", key, "err", err)
		return fallback
	}

	number, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return number
}

// isTruthy menafsirkan nilai checkbox/form sebagai benar.
func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes", "ya":
		return true
	default:
		return false
	}
}
