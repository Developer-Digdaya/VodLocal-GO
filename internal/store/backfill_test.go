package store

import (
	"context"
	"testing"
)

func TestBackfillGenerationItems(t *testing.T) {
	s := newMigratedStore(t)
	ctx := context.Background()

	for _, query := range []string{
		`INSERT INTO providers (id, code, name) VALUES (1, 'dramahits', 'Dramahits')`,
		`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
		 VALUES (10, 1, 'series-10', 'series', 'Series Lama', 'series-lama')`,
		`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
		 VALUES (100, 10, 'ep-100', 1, 1, 'Episode 1')`,
		`INSERT INTO playables (id, title_id, episode_id, duration_seconds)
		 VALUES (1000, 10, 100, 120)`,
		`INSERT INTO generations (id, label, root_path, status)
		 VALUES (41, 'gen-000041', '/media/gen-000041', 'retired')`,
		`INSERT INTO renditions (playable_id, generation_id, quality, playlist_path, status)
		 VALUES (1000, 41, '360p', 'dramahits/series/10/ep-100/360p/index.m3u8', 'ready')`,
	} {
		if _, err := s.Write().ExecContext(ctx, query); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}

	n, err := s.BackfillGenerationItems(ctx)
	if err != nil {
		t.Fatalf("BackfillGenerationItems: %v", err)
	}
	if n != 1 {
		t.Fatalf("baris backfill = %d, mau 1", n)
	}

	var status, category string
	if err := s.Read().QueryRowContext(ctx,
		`SELECT status, category FROM generation_items WHERE generation_id = 41`,
	).Scan(&status, &category); err != nil {
		t.Fatalf("baca generation_items: %v", err)
	}
	if status != "done" || category != "short" {
		t.Fatalf("status/category = %q/%q, mau done/short", status, category)
	}

	n, err = s.BackfillGenerationItems(ctx)
	if err != nil {
		t.Fatalf("BackfillGenerationItems kedua: %v", err)
	}
	if n != 0 {
		t.Fatalf("baris backfill kedua = %d, mau 0", n)
	}
}
