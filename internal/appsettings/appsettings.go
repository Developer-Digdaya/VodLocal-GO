// Package appsettings membaca setting runtime dari tabel app_settings.
package appsettings

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// MaxShortSideKey adalah kunci batas sisi pendek varian video.
const MaxShortSideKey = "sync.max_short_side"

// DefaultMaxShortSide adalah batas default untuk tablet (720p).
const DefaultMaxShortSide = 720

// MaxShortSide membaca batas sisi pendek dari database. Nilai kosong, tidak
// valid, atau di luar rentang wajar memakai default 720.
func MaxShortSide(ctx context.Context, db *sql.DB) int {
	if db == nil {
		return DefaultMaxShortSide
	}
	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = ?`, MaxShortSideKey).Scan(&raw)
	if err != nil {
		return DefaultMaxShortSide
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 144 || value > 1080 {
		return DefaultMaxShortSide
	}
	return value
}
