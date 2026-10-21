package store

import (
	"context"
	"errors"
	"fmt"
)

// orphanCleanupSteps adalah urutan perintah yang dijalankan CleanupOrphans.
// Urutannya penting: playable dibuang lebih dulu, lalu episode yang tidak lagi
// punya playable, terakhir title yang tidak lagi punya playable. Tabel
// providers (dan tabel lain di luar katalog) tidak pernah disentuh.
var orphanCleanupSteps = []struct {
	table string
	query string
}{
	{
		table: "playables",
		query: `DELETE FROM playables
 WHERE id NOT IN (SELECT DISTINCT playable_id FROM renditions)`,
	},
	{
		table: "episodes",
		query: `DELETE FROM episodes
 WHERE id NOT IN (SELECT DISTINCT episode_id FROM playables WHERE episode_id IS NOT NULL)`,
	},
	{
		table: "titles",
		query: `DELETE FROM titles
 WHERE id NOT IN (SELECT DISTINCT title_id FROM playables)`,
	},
}

// CleanupOrphans menghapus playable/episode/title yang tidak lagi punya
// rendition. Mengembalikan jumlah baris yang dihapus.
//
// Seluruh penghapusan berjalan dalam satu transaksi: bila salah satu perintah
// gagal, tidak ada baris yang berubah. Pemanggilan berulang aman — pada data
// yang sama, panggilan kedua tidak menghapus apa pun (deleted = 0).
//
// Fungsi ini dipanggil setelah sebuah generation dihapus (dari panel admin,
// ingest, syncjob, atau boot server) supaya baris katalog yang medianya sudah
// tidak ada tidak tertinggal di database. Baris terhapus mengikuti cascade
// skema: episode dan playable milik title yang dihapus ikut hilang.
func (s *Store) CleanupOrphans(ctx context.Context) (deleted int64, err error) {
	if s == nil || s.write == nil {
		return 0, errors.New("store: CleanupOrphans dipanggil pada Store yang sudah ditutup")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: mulai transaksi cleanup orphans: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, step := range orphanCleanupSteps {
		res, execErr := tx.ExecContext(ctx, step.query)
		if execErr != nil {
			return 0, fmt.Errorf("store: hapus %s tanpa media: %w", step.table, execErr)
		}

		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return 0, fmt.Errorf("store: hitung %s terhapus: %w", step.table, rowsErr)
		}
		deleted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit cleanup orphans: %w", err)
	}

	return deleted, nil
}
