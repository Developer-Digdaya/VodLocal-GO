// Package devseed mengisi database dan media root dengan data sintetis yang
// cukup untuk menguji server secara end-to-end (termasuk uji beban 100 klien)
// tanpa memanggil API provider asli.
//
// Satu kali Run membangun satu generation media lengkap dengan cara yang sama
// seperti syncjob: berkas ditulis ke direktori staging mediafs, diterbitkan
// secara atomik lewat Build.Commit, lalu dicatat ke tabel providers, titles,
// episodes, playables, renditions dan subtitles dalam satu transaksi. Generation
// baru langsung diaktifkan dan generation aktif sebelumnya di-retire, sehingga
// server selalu melihat tepat satu generation aktif.
//
// On-disk layout per episode (relatif terhadap root generation):
//
//	<provider>/series/<external_id>/s01eXX/<quality>/index.m3u8
//	<provider>/series/<external_id>/s01eXX/<quality>/seg-00001.ts
//	<provider>/series/<external_id>/s01eXX/subtitle-id.vtt
//
// Isi setiap berkas deterministik (segmen memakai pola byte 0..255 berulang),
// sehingga hasil seed bisa dibandingkan antar mesin dan tidak ada satu pun
// request jaringan yang dilakukan.
package devseed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"vodlocal/internal/mediafs"
	"vodlocal/internal/store"
)

// Nilai default yang dipakai saat field Config bersangkutan kosong.
const (
	defaultTitles      = 2
	defaultEpisodes    = 2
	defaultSegments    = 3
	defaultSegmentSize = 64 * 1024
	defaultQuality     = "480p"
	defaultProvider    = "dev"

	// providerName adalah kolom providers.name untuk provider sintetis.
	providerName = "Development Provider"
)

// Bentuk data sintetis.
const (
	// generationLabelFormat mengikuti format label syncjob: "gen-" + 6 digit.
	generationLabelFormat = "gen-%06d"
	// titleExternalIDFormat / titleSlugFormat dipakai untuk external_id dan slug.
	titleExternalIDFormat = "series-%03d"
	titleSlugFormat       = "dev-series-%03d"
	titleNameFormat       = "Development Series %02d"
	// episodeExternalIDFormat dipakai untuk external_id episode (season 1).
	episodeExternalIDFormat = "s01e%02d"
	// episodeDirFormat adalah nama direktori episode.
	episodeDirFormat = "s01e%02d"

	seriesDirName    = "series"
	playlistFileName = "index.m3u8"
	segmentPrefix    = "seg-"
	segmentExt       = ".ts"
	subtitleFileName = "subtitle-id.vtt"
	subtitleLang     = "id"
	subtitleLabel    = "Indonesia"
	subtitleFormat   = "vtt"

	// segmentDuration adalah durasi tiap segmen (detik) di playlist sintetis.
	segmentDuration = 4
	// targetDuration adalah nilai #EXT-X-TARGETDURATION pada playlist.
	targetDuration = segmentDuration
	// playlistVersion adalah nilai #EXT-X-VERSION pada playlist.
	playlistVersion = 3

	// Nilai kolom renditions untuk setiap rendition sintetis.
	renditionBitrateKbps = 1000
	renditionWidth       = 854
	renditionHeight      = 480
	renditionContainer   = "ts"
	// sourceURLPrefix adalah awalan source_url (audit URL asli provider).
	sourceURLPrefix = "http://dev.local/"

	// dirMode dan fileMode dipakai untuk seluruh berkas yang dibuat.
	dirMode  os.FileMode = 0o755
	fileMode os.FileMode = 0o644
)

// Config mengatur data yang dibuat Run. Field yang bernilai nol memakai default.
type Config struct {
	// DBPath adalah path file SQLite; wajib.
	DBPath string
	// MediaRoot adalah root direktori generation mediafs; wajib.
	MediaRoot string
	// Titles adalah jumlah series sintetis (default 2).
	Titles int
	// Episodes adalah jumlah episode per series (default 2).
	Episodes int
	// Segments adalah jumlah segmen per rendition (default 3).
	Segments int
	// SegmentSize adalah ukuran satu berkas segmen dalam byte (default 64 KiB).
	SegmentSize int
	// Quality adalah label kualitas rendition (default "480p").
	Quality string
	// Provider adalah kode provider sintetis (default "dev").
	Provider string
}

// withDefaults mengisi setiap field Config yang kosong dengan nilai default.
func (c Config) withDefaults() Config {
	if c.Titles <= 0 {
		c.Titles = defaultTitles
	}
	if c.Episodes <= 0 {
		c.Episodes = defaultEpisodes
	}
	if c.Segments <= 0 {
		c.Segments = defaultSegments
	}
	if c.SegmentSize <= 0 {
		c.SegmentSize = defaultSegmentSize
	}
	if strings.TrimSpace(c.Quality) == "" {
		c.Quality = defaultQuality
	}
	if strings.TrimSpace(c.Provider) == "" {
		c.Provider = defaultProvider
	}
	c.DBPath = strings.TrimSpace(c.DBPath)
	c.MediaRoot = strings.TrimSpace(c.MediaRoot)
	c.Quality = strings.TrimSpace(c.Quality)
	c.Provider = strings.TrimSpace(c.Provider)
	return c
}

// validate menolak konfigurasi yang tidak bisa dipakai.
func (c Config) validate() error {
	if c.DBPath == "" {
		return errors.New("devseed: Config.DBPath wajib diisi")
	}
	if c.MediaRoot == "" {
		return errors.New("devseed: Config.MediaRoot wajib diisi")
	}
	return nil
}

// Result merangkum satu kali Run.
type Result struct {
	// Generation adalah label generation yang baru diterbitkan, mis. "gen-000007".
	Generation string
	// ProviderID adalah id baris providers untuk Config.Provider.
	ProviderID int64
	// TitleIDs adalah id titles yang dibuat/diperbarui, berurutan sesuai indeks.
	TitleIDs []int64
	// PlayableIDs adalah id playables, satu per episode.
	PlayableIDs []int64
	// PlaylistPaths adalah playlist_path relatif terhadap root generation,
	// sejajar indeks dengan PlayableIDs.
	PlaylistPaths []string
}

// Run membuat satu generation data sintetis beserta katalognya.
//
// Langkahnya: buka dan migrasikan database, siapkan media root, tentukan label
// generation berikutnya, tulis seluruh berkas ke staging, terbitkan staging,
// catat katalog dalam satu transaksi, lalu aktifkan generation baru dan
// retire generation aktif sebelumnya dalam transaksi kedua.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return Result{}, err
	}

	res := Result{}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return res, fmt.Errorf("devseed: buka store: %w", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx); err != nil {
		return res, fmt.Errorf("devseed: migrasi database: %w", err)
	}

	media, err := mediafs.New(cfg.MediaRoot)
	if err != nil {
		return res, fmt.Errorf("devseed: siapkan media root: %w", err)
	}

	genID, err := nextGenerationID(ctx, st)
	if err != nil {
		return res, err
	}
	label := fmt.Sprintf(generationLabelFormat, genID)
	res.Generation = label

	build, err := media.NewStaging(label)
	if err != nil {
		return res, fmt.Errorf("devseed: mulai staging %s: %w", label, err)
	}
	// Staging hanya dibuang bila belum pernah di-commit.
	defer func() {
		if !build.Closed() {
			_ = build.Abort()
		}
	}()

	plan, err := writeGeneration(build.Dir(), cfg)
	if err != nil {
		return res, err
	}

	if err := build.Commit(); err != nil {
		return res, fmt.Errorf("devseed: commit generation %s: %w", label, err)
	}

	genDir, err := media.GenerationDir(label)
	if err != nil {
		return res, fmt.Errorf("devseed: direktori generation %s: %w", label, err)
	}
	files, bytes, err := dirStats(genDir)
	if err != nil {
		return res, fmt.Errorf("devseed: hitung isi generation %s: %w", label, err)
	}

	now := stamp()
	if err := insertCatalog(ctx, st, genID, res.Generation, cfg, plan, now, &res); err != nil {
		// Database tidak mencatat generation ini, jadi direktori yang sudah
		// diterbitkan tidak ada gunanya: hapus supaya seed bisa diulang.
		_ = media.Remove(label)
		return Result{}, err
	}

	if err := activateGeneration(ctx, st, genID, label, cfg.Provider, now, files, bytes); err != nil {
		return res, err
	}

	return res, nil
}

// mediaPlan adalah hasil penulisan berkas satu generation.
type mediaPlan struct {
	titles []titlePlan
}

// titlePlan menyimpan satu series yang berkasnya sudah ditulis.
type titlePlan struct {
	externalID string
	slug       string
	name       string
	episodes   []episodePlan
}

// episodePlan menyimpan satu episode yang berkasnya sudah ditulis.
type episodePlan struct {
	externalID    string
	number        int
	name          string
	playlistRel   string
	subtitleRel   string
	subtitleBytes int64
	segmentCount  int
	totalBytes    int64
	duration      int
}

// writeGeneration menulis playlist, segmen dan subtitle seluruh episode ke root
// staging, lalu mengembalikan rencana yang dipakai saat mengisi database.
func writeGeneration(root string, cfg Config) (mediaPlan, error) {
	providerDir := safeComponent(cfg.Provider)
	qualityDir := safeComponent(cfg.Quality)

	plan := mediaPlan{titles: make([]titlePlan, 0, cfg.Titles)}

	for i := 1; i <= cfg.Titles; i++ {
		title := titlePlan{
			externalID: fmt.Sprintf(titleExternalIDFormat, i),
			slug:       fmt.Sprintf(titleSlugFormat, i),
			name:       fmt.Sprintf(titleNameFormat, i),
		}
		base := path.Join(providerDir, seriesDirName, title.externalID)
		title.episodes = make([]episodePlan, 0, cfg.Episodes)

		for ep := 1; ep <= cfg.Episodes; ep++ {
			epName := fmt.Sprintf(episodeDirFormat, ep)
			episode := episodePlan{
				externalID:   fmt.Sprintf(episodeExternalIDFormat, ep),
				number:       ep,
				name:         fmt.Sprintf("Episode %02d", ep),
				playlistRel:  path.Join(base, epName, qualityDir, playlistFileName),
				subtitleRel:  path.Join(base, epName, subtitleFileName),
				segmentCount: cfg.Segments,
				duration:     cfg.Segments * segmentDuration,
			}

			renditionRel := path.Dir(episode.playlistRel)
			renditionAbs := filepath.Join(root, filepath.FromSlash(renditionRel))
			if err := os.MkdirAll(renditionAbs, dirMode); err != nil {
				return mediaPlan{}, fmt.Errorf("devseed: buat direktori rendition %s: %w", renditionRel, err)
			}

			playlist := buildPlaylist(cfg.Segments)
			if err := os.WriteFile(filepath.Join(renditionAbs, playlistFileName), playlist, fileMode); err != nil {
				return mediaPlan{}, fmt.Errorf("devseed: tulis playlist %s: %w", episode.playlistRel, err)
			}
			episode.totalBytes = int64(len(playlist))

			for seg := 1; seg <= cfg.Segments; seg++ {
				data := segmentBytes(cfg.SegmentSize, seg)
				name := segmentName(seg)
				if err := os.WriteFile(filepath.Join(renditionAbs, name), data, fileMode); err != nil {
					return mediaPlan{}, fmt.Errorf("devseed: tulis segmen %s: %w", path.Join(renditionRel, name), err)
				}
				episode.totalBytes += int64(len(data))
			}

			subtitle := buildSubtitle(title.name, ep)
			subtitleAbs := filepath.Join(root, filepath.FromSlash(episode.subtitleRel))
			if err := os.MkdirAll(filepath.Dir(subtitleAbs), dirMode); err != nil {
				return mediaPlan{}, fmt.Errorf("devseed: buat direktori subtitle %s: %w", episode.subtitleRel, err)
			}
			if err := os.WriteFile(subtitleAbs, subtitle, fileMode); err != nil {
				return mediaPlan{}, fmt.Errorf("devseed: tulis subtitle %s: %w", episode.subtitleRel, err)
			}
			episode.subtitleBytes = int64(len(subtitle))

			title.episodes = append(title.episodes, episode)
		}

		plan.titles = append(plan.titles, title)
	}

	return plan, nil
}

// buildPlaylist menyusun media playlist HLS valid untuk segments segmen.
func buildPlaylist(segments int) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:" + itoa(playlistVersion) + "\n")
	b.WriteString("#EXT-X-TARGETDURATION:" + itoa(targetDuration) + "\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for seg := 1; seg <= segments; seg++ {
		// Durasi ditulis satu desimal supaya tetap "4.0" seperti contoh dokumen.
		b.WriteString("#EXTINF:4.0,\n")
		b.WriteString(segmentName(seg))
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// buildSubtitle menyusun berkas subtitle WebVTT sederhana untuk satu episode.
func buildSubtitle(title string, episode int) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	b.WriteString("00:00:00.000 --> 00:00:04.000\n")
	b.WriteString(title + " episode " + itoa(episode) + "\n")
	return []byte(b.String())
}

// segmentName mengembalikan nama berkas segmen ke-n, mis. "seg-00001.ts".
func segmentName(n int) string {
	return fmt.Sprintf("%s%05d%s", segmentPrefix, n, segmentExt)
}

// segmentBytes menyusun isi segmen sintetis: pola byte 0..255 yang digeser seed
// sehingga deterministik tetapi berbeda antar segmen.
func segmentBytes(size, seed int) []byte {
	if size < 1 {
		size = 1
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i + seed) % 256)
	}
	return buf
}

// insertCatalog menulis generation, provider, title, episode, playable,
// rendition dan subtitle dalam satu transaksi.
func insertCatalog(ctx context.Context, st *store.Store, genID int64, label string, cfg Config, plan mediaPlan, now string, res *Result) error {
	tx, err := st.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("devseed: mulai transaksi katalog: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Baris generations dibuat di sini supaya renditions bisa mereferensikannya
	// dalam transaksi yang sama. root_path disimpan relatif (labelnya sendiri).
	if _, err := tx.ExecContext(ctx, `
INSERT INTO generations (id, label, root_path, status, created_at)
VALUES (?, ?, ?, 'building', ?)`,
		genID, label, label, now,
	); err != nil {
		return fmt.Errorf("devseed: simpan generation %s: %w", label, err)
	}

	provID, err := upsertProvider(ctx, tx, cfg.Provider, now)
	if err != nil {
		return err
	}
	res.ProviderID = provID

	res.TitleIDs = make([]int64, 0, len(plan.titles))
	res.PlayableIDs = make([]int64, 0, len(plan.titles)*cfg.Episodes)
	res.PlaylistPaths = make([]string, 0, len(plan.titles)*cfg.Episodes)

	for _, title := range plan.titles {
		titleID, err := upsertTitle(ctx, tx, provID, title, now)
		if err != nil {
			return err
		}
		res.TitleIDs = append(res.TitleIDs, titleID)

		for _, episode := range title.episodes {
			episodeID, err := upsertEpisode(ctx, tx, titleID, episode, now)
			if err != nil {
				return err
			}

			playableID, err := upsertPlayable(ctx, tx, titleID, episodeID, episode, now)
			if err != nil {
				return err
			}
			res.PlayableIDs = append(res.PlayableIDs, playableID)
			res.PlaylistPaths = append(res.PlaylistPaths, episode.playlistRel)

			if err := insertRendition(ctx, tx, playableID, genID, cfg.Quality, episode, now); err != nil {
				return err
			}
			if err := insertSubtitle(ctx, tx, playableID, genID, episode); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("devseed: commit katalog: %w", err)
	}
	return nil
}

// upsertProvider memastikan baris providers ada dan mengembalikan id-nya.
func upsertProvider(ctx context.Context, tx *sql.Tx, code, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO providers (code, name, is_enabled, created_at, updated_at)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT(code) DO UPDATE SET
    name       = excluded.name,
    is_enabled = 1,
    updated_at = excluded.updated_at
RETURNING id`,
		code, providerName, now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("devseed: simpan provider %q: %w", code, err)
	}
	return id, nil
}

// upsertTitle menyimpan satu series (duration_seconds NULL) dan mengembalikan
// id-nya. Seed ulang tidak menduplikasi katalog karena external_id tetap.
func upsertTitle(ctx context.Context, tx *sql.Tx, provID int64, title titlePlan, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO titles (provider_id, external_id, kind, title, slug, language, duration_seconds,
                    is_published, created_at, updated_at)
VALUES (?, ?, 'series', ?, ?, 'id', NULL, 1, ?, ?)
ON CONFLICT(provider_id, external_id) DO UPDATE SET
    kind             = 'series',
    title            = excluded.title,
    slug             = excluded.slug,
    language         = excluded.language,
    duration_seconds = NULL,
    is_published     = 1,
    updated_at       = excluded.updated_at
RETURNING id`,
		provID, title.externalID, title.name, title.slug, now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("devseed: simpan title %q: %w", title.externalID, err)
	}
	return id, nil
}

// upsertEpisode menyimpan episode season 1 dan mengembalikan id-nya.
func upsertEpisode(ctx context.Context, tx *sql.Tx, titleID int64, episode episodePlan, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO episodes (title_id, external_id, season_number, episode_number, name,
                      duration_seconds, created_at, updated_at)
VALUES (?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(title_id, season_number, episode_number) DO UPDATE SET
    name             = excluded.name,
    duration_seconds = excluded.duration_seconds,
    updated_at       = excluded.updated_at
RETURNING id`,
		titleID, episode.externalID, episode.number, episode.name,
		episode.duration, now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("devseed: simpan episode %q: %w", episode.externalID, err)
	}
	return id, nil
}

// upsertPlayable memastikan satu playable per episode ada dan mengembalikan
// id-nya. Satu playable dipakai ulang oleh setiap generation berikutnya.
func upsertPlayable(ctx context.Context, tx *sql.Tx, titleID, episodeID int64, episode episodePlan, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM playables WHERE episode_id = ?`, episodeID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.QueryRowContext(ctx, `
INSERT INTO playables (title_id, episode_id, duration_seconds, is_encrypted, created_at, updated_at)
VALUES (?, ?, ?, 0, ?, ?) RETURNING id`,
			titleID, episodeID, episode.duration, now, now,
		).Scan(&id); err != nil {
			return 0, fmt.Errorf("devseed: simpan playable %q: %w", episode.externalID, err)
		}
		return id, nil
	case err != nil:
		return 0, fmt.Errorf("devseed: cari playable %q: %w", episode.externalID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE playables SET duration_seconds = ?, updated_at = ? WHERE id = ?`,
		episode.duration, now, id,
	); err != nil {
		return 0, fmt.Errorf("devseed: perbarui playable %q: %w", episode.externalID, err)
	}
	return id, nil
}

// insertRendition mencatat satu rendition yang berkasnya sudah lengkap dan
// langsung berstatus 'ready'.
func insertRendition(ctx context.Context, tx *sql.Tx, playableID, genID int64, quality string, episode episodePlan, now string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO renditions (playable_id, generation_id, quality, bitrate_kbps, width, height,
                        container, source_url, playlist_path, segment_count, segments_done,
                        total_bytes, bytes_done, duration_seconds, status, verified_at,
                        created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?, ?)`,
		playableID, genID, quality, renditionBitrateKbps, renditionWidth, renditionHeight,
		renditionContainer, sourceURLPrefix+episode.playlistRel, episode.playlistRel,
		episode.segmentCount, episode.segmentCount, episode.totalBytes, episode.totalBytes,
		episode.duration, now, now, now,
	); err != nil {
		return fmt.Errorf("devseed: simpan rendition %q: %w", episode.playlistRel, err)
	}
	return nil
}

// insertSubtitle mencatat subtitle Indonesia yang berkasnya sudah ditulis.
func insertSubtitle(ctx context.Context, tx *sql.Tx, playableID, genID int64, episode episodePlan) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO subtitles (playable_id, generation_id, lang, label, format, file_path, bytes, status)
VALUES (?, ?, ?, ?, ?, ?, ?, 'ready')`,
		playableID, genID, subtitleLang, subtitleLabel, subtitleFormat,
		episode.subtitleRel, episode.subtitleBytes,
	); err != nil {
		return fmt.Errorf("devseed: simpan subtitle %q: %w", episode.subtitleRel, err)
	}
	return nil
}

// activateGeneration mengaktifkan generation baru dan me-retire generation
// aktif sebelumnya. Urutannya retire lebih dulu supaya unique index
// ux_generations_one_active tidak dilanggar.
func activateGeneration(ctx context.Context, st *store.Store, genID int64, label, provider, now string, files int, bytes int64) error {
	tx, err := st.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("devseed: mulai transaksi aktivasi: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
UPDATE generations
   SET status = 'retired', retired_at = ?
 WHERE status = 'active' AND id <> ?`,
		now, genID,
	); err != nil {
		return fmt.Errorf("devseed: retire generation lama: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE generations
   SET status = 'active', activated_at = ?, file_count = ?, total_bytes = ?, note = ?
 WHERE id = ?`,
		now, files, bytes, "development seed ("+provider+")", genID,
	); err != nil {
		return fmt.Errorf("devseed: aktifkan generation %s: %w", label, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("devseed: commit aktivasi generation %s: %w", label, err)
	}
	return nil
}

// nextGenerationID mengembalikan MAX(id)+1 pada tabel generations (1 bila
// tabelnya masih kosong).
func nextGenerationID(ctx context.Context, st *store.Store) (int64, error) {
	var maxID sql.NullInt64
	if err := st.Read().QueryRowContext(ctx,
		`SELECT MAX(id) FROM generations`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("devseed: baca MAX(id) generations: %w", err)
	}
	if !maxID.Valid {
		return 1, nil
	}
	return maxID.Int64 + 1, nil
}

// dirStats menghitung jumlah berkas biasa dan total ukurannya di dir (rekursif).
func dirStats(dir string) (files int, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return files, bytes, nil
}

// stamp mengembalikan waktu sekarang sebagai TEXT ISO-8601 UTC.
func stamp() string { return time.Now().UTC().Format(time.RFC3339) }

// itoa memformat angka menjadi string desimal.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// safeComponent mengubah sebuah nilai menjadi satu komponen path yang aman,
// sehingga kode provider atau label kualitas tidak pernah bisa keluar dari
// direktori generation.
func safeComponent(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == '-', r == '_':
			if b.Len() > 0 && !dash {
				b.WriteRune(r)
				dash = true
			}
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "x"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-_")
		if out == "" {
			return "x"
		}
	}
	return out
}
