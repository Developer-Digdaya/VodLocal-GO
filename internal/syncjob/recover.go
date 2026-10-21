package syncjob

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// powerLossNote adalah error_message yang dipasang Recover pada run yang
// terputus. Nilainya tetap supaya admin bisa membedakannya dari kegagalan biasa.
const powerLossNote = "power loss"

// Recover membereskan sisa-sisa proses yang mati mendadak (mis. listrik padam):
//
//   - sync_runs yang masih 'queued'/'running'/'verifying'/'switching'/'cleanup'
//     ditandai 'failed' dengan error_message "power loss",
//   - generations yang masih 'building' ditandai 'failed',
//   - direktori staging ".building-*" di root media dihapus.
//
// Fungsi ini idempoten sehingga aman dipanggil setiap kali aplikasi start.
func (s *Syncer) Recover(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	now := s.stamp()

	res, err := s.store.Write().ExecContext(ctx, `
UPDATE sync_runs
   SET status = 'failed', error_message = ?, finished_at = ?
 WHERE status IN ('queued','running','verifying','switching','cleanup')`,
		powerLossNote, now,
	)
	if err != nil {
		return fmt.Errorf("syncjob: pulihkan sync_runs: %w", err)
	}
	runs, _ := res.RowsAffected()

	res, err = s.store.Write().ExecContext(ctx, `
UPDATE generations
   SET status = 'failed', note = ?
 WHERE status = 'building'`,
		powerLossNote,
	)
	if err != nil {
		return fmt.Errorf("syncjob: pulihkan generations: %w", err)
	}
	gens, _ := res.RowsAffected()

	removed, err := s.removeStaging()
	if err != nil {
		return err
	}

	s.log.Info("recover selesai",
		"runs_ditandai_gagal", runs,
		"generations_ditandai_gagal", gens,
		"staging_dihapus", removed,
	)
	return nil
}

// removeStaging menghapus seluruh direktori ".building-*" di bawah root media.
func (s *Syncer) removeStaging() (int, error) {
	root := s.media.Root()

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("syncjob: baca root media %q: %w", root, err)
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		p := filepath.Join(root, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return removed, fmt.Errorf("syncjob: hapus staging %q: %w", p, err)
		}
		removed++
	}
	return removed, nil
}
