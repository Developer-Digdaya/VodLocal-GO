package syncjob

import (
	"context"
	"testing"
)

// TestRunMembersihkanKatalogTanpaMedia memastikan run yang berhasil ikut
// membuang playable/episode/title lama yang tidak lagi punya rendition, tanpa
// mengganggu katalog yang baru ditulis run tersebut.
func TestRunMembersihkanKatalogTanpaMedia(t *testing.T) {
	srv := mediaServer(t, 2)
	st := newTestStore(t)
	rig := newRig(t, st, srv.Client(), movieProvider(srv.URL+"/hls/index.m3u8"))

	// Sisa katalog yang generationnya sudah hilang: provider, title series,
	// episode, dan playable tanpa satu pun rendition.
	seed := []string{
		`INSERT INTO providers (id, code, name) VALUES (9, 'lama', 'Provider Lama')`,
		`INSERT INTO titles (id, provider_id, external_id, kind, title, slug)
		 VALUES (50, 9, 'series-lama', 'series', 'Series Lama', 'series-lama')`,
		`INSERT INTO episodes (id, title_id, external_id, season_number, episode_number, name)
		 VALUES (60, 50, 'ep-lama', 1, 1, 'Episode Lama')`,
		`INSERT INTO playables (id, title_id, episode_id, duration_seconds) VALUES (70, 50, 60, 600)`,
	}
	for _, query := range seed {
		if _, err := st.Write().Exec(query); err != nil {
			t.Fatalf("seed gagal: %v\nquery: %s", err, query)
		}
	}

	res, err := rig.syncer.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM playables WHERE id = 70`); got != 0 {
		t.Errorf("playable tanpa media masih ada setelah sync")
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM episodes WHERE id = 60`); got != 0 {
		t.Errorf("episode tanpa media masih ada setelah sync")
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM titles WHERE id = 50`); got != 0 {
		t.Errorf("title tanpa media masih ada setelah sync")
	}

	// Katalog hasil run tidak ikut tersapu.
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM titles`); got != int64(res.Titles) {
		t.Errorf("jumlah titles = %d, mau %d (hasil run)", got, res.Titles)
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM playables`); got != int64(res.Playables) {
		t.Errorf("jumlah playables = %d, mau %d (hasil run)", got, res.Playables)
	}
	if got := scanInt(t, st.Read(), `SELECT COUNT(*) FROM providers WHERE id = 9`); got != 1 {
		t.Errorf("provider lama ikut terhapus")
	}
}
