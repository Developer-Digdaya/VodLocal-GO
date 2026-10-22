package ingest

import (
	"context"
	"fmt"

	"vodlocal/internal/catalog"
)

// Nilai kolom generation_items.status yang ditulis paket ini. 'skipped' tidak
// pernah dipakai karena setiap selection selalu dicoba diproses.
const (
	itemPending = "pending"
	itemRunning = "running"
	itemDone    = "done"
	itemFailed  = "failed"
)

// Nilai kolom generation_items.current_step.
const (
	stepEpisodes = "episodes"
	stepPoster   = "poster"
	stepDownload = "download"
	stepPlaylist = "playlist"
	stepSegments = "segments"
	stepDone     = "done"
)

// seriesProgress melaporkan progres satu series ke tabel generation_items
// sehingga UI bisa menampilkan persentase dan status per series.
//
// Satu seriesProgress hanya dipakai oleh goroutine run, jadi counter di
// dalamnya tidak perlu dikunci. Seluruh penulisan bersifat best effort:
// kegagalan database hanya dicatat sebagai log Warn dan tidak pernah
// menggagalkan run (progres bukan bagian dari kebenaran data media).
type seriesProgress struct {
	svc      *Service
	genID    int64
	provID   int64
	external string

	// Counter yang dilaporkan ke generation_items.
	total         int
	done          int
	failed        int
	renditionsOK  int
	renditionsBad int
	bytes         int64

	// lastReportedPercent mencegah UPDATE berlebihan saat callback per segmen.
	lastReportedPercent int
}

// episodeResult merangkum hasil satu episode untuk pembukuan progres series.
type episodeResult struct {
	// attempted menandai episode punya stream yang benar-benar dicoba
	// disiapkan, sehingga kegagalannya terhitung sebagai rendition gagal.
	attempted bool
	// ready menandai rendition episode berstatus 'ready'.
	ready bool
	// bytes adalah jumlah byte yang terunduh untuk episode ini (termasuk
	// unduhan yang berakhir gagal).
	bytes int64
}

// seedGenerationItems menyisipkan satu baris generation_items 'pending' untuk
// setiap selection pada run ini. Penyisipan idempotent
// (ON CONFLICT DO NOTHING), jadi mengulang run tidak menimpa progres yang sudah
// ada. Kegagalan penyisipan hanya dicatat di log.
func (s *Service) seedGenerationItems(ctx context.Context, st *runState, provIDs map[string]int64) {
	ctx, cancel := s.sqlCtx(ctx)
	defer cancel()

	now := s.stamp()
	for _, series := range st.selections {
		provID, ok := provIDs[normalizeProvider(series.Provider)]
		if !ok {
			s.log.Warn("selection tanpa provider id, generation item dilewati",
				"generation_id", st.genID, "provider", series.Provider)
			continue
		}
		external := seriesExternalID(series)
		if external == "" {
			s.log.Warn("selection tanpa slug/id, generation item dilewati",
				"generation_id", st.genID)
			continue
		}

		if _, err := s.store.Write().ExecContext(ctx, `
INSERT INTO generation_items (generation_id, provider_id, category, series_external_id,
                              series_title, poster_url, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(generation_id, provider_id, series_external_id) DO NOTHING`,
			st.genID, provID, seriesCategory(series), external,
			firstNonEmpty(series.Title, external), nullString(series.PosterURL),
			itemPending, now, now,
		); err != nil {
			s.log.Warn("gagal mencatat generation item",
				"generation_id", st.genID, "series", external, "error", err)
		}
	}
}

// newSeriesProgress menyiapkan pelapor progres satu series. Tidak ada baris
// yang disentuh sampai salah satu metodenya dipanggil.
func (s *Service) newSeriesProgress(st *runState, provID int64, series catalog.Series) *seriesProgress {
	return &seriesProgress{
		svc:      s,
		genID:    st.genID,
		provID:   provID,
		external: seriesExternalID(series),
	}
}

// itemStep mencatat tahap series yang sedang diproses. Aman dipanggil walau
// tidak ada series yang sedang dilaporkan.
func (s *Service) itemStep(ctx context.Context, st *runState, step string) {
	if st == nil || st.item == nil {
		return
	}
	st.item.step(ctx, step)
}

// start menandai series mulai diproses. started_at yang sudah terisi tidak
// ditimpa supaya jejak percobaan pertama tetap terlihat.
func (p *seriesProgress) start(ctx context.Context) {
	now := p.svc.stamp()
	p.exec(ctx, `
UPDATE generation_items
   SET status = ?, started_at = COALESCE(started_at, ?), current_step = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		itemRunning, now, stepEpisodes, now, p.genID, p.provID, p.external)
}

// step mencatat tahap yang sedang dikerjakan series ini.
func (p *seriesProgress) step(ctx context.Context, step string) {
	p.exec(ctx, `
UPDATE generation_items
   SET current_step = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		step, p.svc.stamp(), p.genID, p.provID, p.external)
}

// setTotal mencatat jumlah episode yang akan diproses dan berpindah ke tahap
// unduhan. Jumlahnya adalah episode yang benar-benar diproses
// (sudah dibatasi Config.MaxEpisodes) supaya progres bisa mencapai 100%.
func (p *seriesProgress) setTotal(ctx context.Context, total int) {
	p.total = total
	p.lastReportedPercent = progressPercent(p.processed(), total)
	p.exec(ctx, `
UPDATE generation_items
   SET total_episodes = ?, current_step = ?, progress_percent = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		total, stepDownload, p.lastReportedPercent, p.svc.stamp(),
		p.genID, p.provID, p.external)
}

// episodeStart menandai episode yang sedang diunduh beserta tahap awalnya.
func (p *seriesProgress) episodeStart(ctx context.Context, season, number int) {
	p.exec(ctx, `
UPDATE generation_items
   SET current_episode = ?, current_step = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		episodeLabel(season, number), stepPlaylist, p.svc.stamp(),
		p.genID, p.provID, p.external)
}

// episodeDone menambahkan hasil satu episode ke counter lalu menulis satu baris
// pembaruan. Progres di tengah episode sudah dilaporkan terpisah oleh
// segmentProgress.
func (p *seriesProgress) episodeDone(ctx context.Context, res episodeResult) {
	switch {
	case res.attempted && res.ready:
		p.done++
		p.renditionsOK++
	case res.attempted:
		p.failed++
		p.renditionsBad++
	default:
		// Episode tanpa stream: tidak ada rendition yang dicoba, tetap
		// dihitung sebagai episode gagal supaya progres bisa tuntas.
		p.failed++
	}
	p.bytes += res.bytes
	p.lastReportedPercent = progressPercent(p.processed(), p.total)

	p.exec(ctx, `
UPDATE generation_items
   SET episodes_done = ?, episodes_failed = ?, renditions_ready = ?, renditions_failed = ?,
       bytes_downloaded = ?, progress_percent = ?, current_step = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		p.done, p.failed, p.renditionsOK, p.renditionsBad, p.bytes,
		p.lastReportedPercent, stepDownload, p.svc.stamp(),
		p.genID, p.provID, p.external)
}

// segmentProgress melaporkan progres di dalam satu episode yang masih
// berjalan. Untuk movie (total episode 1) ini yang membuat progress bar tidak
// tertahan di 0 sampai unduhan selesai. Update hanya dilakukan jika persentase
// berubah, sehingga callback per segmen tidak membanjiri database.
func (p *seriesProgress) segmentProgress(ctx context.Context, done, total int, bytes int64) {
	if p == nil || total < 1 {
		return
	}
	percent := p.downloadPercent(done, total)
	if percent <= p.lastReportedPercent {
		return
	}
	p.lastReportedPercent = percent
	p.exec(ctx, `
UPDATE generation_items
   SET progress_percent = ?, bytes_downloaded = ?, current_step = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		percent, p.bytes+bytes, stepSegments, p.svc.stamp(),
		p.genID, p.provID, p.external)
}

// downloadPercent menghitung persentase gabungan episode selesai + fraksi
// episode yang sedang diunduh. Nilai 100 ditahan di 99 sampai episode benar-
// benar selesai.
func (p *seriesProgress) downloadPercent(done, total int) int {
	episodes := p.total
	if episodes < 1 {
		episodes = 1
	}
	fraction := float64(done) / float64(total)
	percent := int(((float64(p.processed()) + fraction) * 100) / float64(episodes))
	if percent > 99 {
		return 99
	}
	if percent < 0 {
		return 0
	}
	return percent
}

// finish menutup series yang berhasil: status 'done' dan progres 100%.
func (p *seriesProgress) finish(ctx context.Context) {
	now := p.svc.stamp()
	p.lastReportedPercent = 100
	p.exec(ctx, `
UPDATE generation_items
   SET status = ?, progress_percent = 100, current_step = ?, episodes_done = ?,
       episodes_failed = ?, renditions_ready = ?, renditions_failed = ?,
       bytes_downloaded = ?, error_message = NULL, finished_at = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		itemDone, stepDone, p.done, p.failed, p.renditionsOK, p.renditionsBad, p.bytes,
		now, now, p.genID, p.provID, p.external)
}

// fail menutup series yang gagal. Progres dibiarkan apa adanya; hanya status,
// pesan kesalahan, dan waktu selesai yang diisi.
func (p *seriesProgress) fail(ctx context.Context, cause error) {
	var msg any
	if cause != nil {
		msg = truncate(cause.Error(), maxNoteLen)
	}

	now := p.svc.stamp()
	p.exec(ctx, `
UPDATE generation_items
   SET status = ?, error_message = ?, finished_at = ?, updated_at = ?
 WHERE generation_id = ? AND provider_id = ? AND series_external_id = ?`,
		itemFailed, msg, now, now, p.genID, p.provID, p.external)
}

// processed adalah jumlah episode yang sudah diproses (berhasil maupun gagal).
func (p *seriesProgress) processed() int { return p.done + p.failed }

// exec menjalankan satu perintah tulis generation_items. Kegagalan hanya
// dicatat di log: progres tidak pernah menggagalkan run.
func (p *seriesProgress) exec(ctx context.Context, query string, args ...any) {
	ctx, cancel := p.svc.sqlCtx(ctx)
	defer cancel()

	if _, err := p.svc.store.Write().ExecContext(ctx, query, args...); err != nil {
		p.svc.log.Warn("gagal memperbarui progres series",
			"generation_id", p.genID, "provider_id", p.provID,
			"series", p.external, "error", err)
	}
}

// seriesExternalID adalah nilai kolom generation_items.series_external_id:
// slug series, atau id-nya bila slug kosong (sama dengan titles.external_id).
func seriesExternalID(series catalog.Series) string {
	return firstNonEmpty(series.Slug, series.ID)
}

// seriesCategory adalah nilai kolom generation_items.category: kategori
// selection, atau "short" bila kosong.
func seriesCategory(series catalog.Series) string {
	return firstNonEmpty(series.Category, catalog.CategoryShort)
}

// episodeLabel menyusun penanda episode untuk kolom current_episode.
func episodeLabel(season, number int) string {
	return fmt.Sprintf("S%02dE%02d", season, number)
}

// progressPercent menghitung persentase episode yang sudah diproses terhadap
// totalnya dan menjepit hasilnya ke 0..100. Total 0 dianggap 1 supaya tidak
// terjadi pembagian nol.
func progressPercent(processed, total int) int {
	if total < 1 {
		total = 1
	}
	pct := processed * 100 / total
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
