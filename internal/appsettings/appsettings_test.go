package appsettings

import (
	"context"
	"path/filepath"
	"testing"

	"vodlocal/internal/store"
)

func TestMaxShortSide(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if got := MaxShortSide(ctx, st.Read()); got != DefaultMaxShortSide {
		t.Fatalf("default = %d, mau %d", got, DefaultMaxShortSide)
	}

	if _, err := st.Write().ExecContext(ctx,
		`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)`,
		MaxShortSideKey, "480", "2026-10-22T00:00:00Z",
	); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if got := MaxShortSide(ctx, st.Read()); got != 480 {
		t.Fatalf("setting = %d, mau 480", got)
	}

	if _, err := st.Write().ExecContext(ctx,
		`UPDATE app_settings SET value = 'tidak-valid' WHERE key = ?`, MaxShortSideKey,
	); err != nil {
		t.Fatalf("update setting: %v", err)
	}
	if got := MaxShortSide(ctx, st.Read()); got != DefaultMaxShortSide {
		t.Fatalf("invalid = %d, mau default %d", got, DefaultMaxShortSide)
	}
}
