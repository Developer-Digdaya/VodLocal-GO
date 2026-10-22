package store

import (
	"context"
	"fmt"
)

// BackfillGenerationItems memastikan setiap (generation, judul) yang punya
// rendition ready punya satu baris generation_items. Ingest menulis baris ini
// saat memproses selection, sedangkan sync penuh lama tidak; backfill membuat
// unduhan lama tetap terlihat dan bisa dihapus dari panel admin.
//
// Idempotent: baris yang sudah ada tidak diubah. Generation building/failed
// dilewati karena isinya belum/tidak diterbitkan.
func (s *Store) BackfillGenerationItems(ctx context.Context) (int64, error) {
	res, err := s.write.ExecContext(ctx, `
INSERT INTO generation_items (
	generation_id, provider_id, category, series_external_id, series_title,
	status, total_episodes, episodes_done, total_renditions, renditions_ready,
	progress_percent
)
SELECT
	g.id,
	t.provider_id,
	CASE WHEN p.code = 'dramahits' THEN 'short' ELSE t.kind END,
	t.external_id,
	t.title,
	'done',
	CASE WHEN t.kind = 'movie' THEN 1 ELSE (SELECT COUNT(*) FROM episodes e WHERE e.title_id = t.id) END,
	CASE WHEN t.kind = 'movie' THEN 1 ELSE (
		SELECT COUNT(DISTINCT pl2.episode_id)
		FROM renditions r2
		JOIN playables pl2 ON pl2.id = r2.playable_id
		WHERE pl2.title_id = t.id AND r2.generation_id = g.id
		  AND r2.status = 'ready' AND pl2.episode_id IS NOT NULL
	) END,
	COUNT(r.id),
	COUNT(r.id),
	100
FROM generations g
JOIN renditions r ON r.generation_id = g.id AND r.status = 'ready'
JOIN playables pl ON pl.id = r.playable_id
JOIN titles t ON t.id = pl.title_id
JOIN providers p ON p.id = t.provider_id
WHERE g.status NOT IN ('building', 'failed')
  AND NOT EXISTS (
	SELECT 1 FROM generation_items gi
	WHERE gi.generation_id = g.id
	  AND gi.provider_id = t.provider_id
	  AND gi.series_external_id = t.external_id
  )
GROUP BY g.id, t.id`)
	if err != nil {
		return 0, fmt.Errorf("store: backfill generation_items: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: hitung backfill generation_items: %w", err)
	}
	return n, nil
}
