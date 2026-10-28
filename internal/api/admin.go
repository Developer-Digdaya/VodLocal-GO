package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"vodlocal/internal/response"
)

// onlineWindow adalah batas waktu sebuah device dianggap online (10.2/10.4).
const onlineWindow = 5 * time.Minute

// adminGeneration adalah bagian generation pada /admin/overview.
type adminGeneration struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	TotalBytes int64  `json:"total_bytes"`
}

// adminCatalog adalah bagian catalog pada /admin/overview.
type adminCatalog struct {
	Titles          int64 `json:"titles"`
	Episodes        int64 `json:"episodes"`
	Playables       int64 `json:"playables"`
	RenditionsReady int64 `json:"renditions_ready"`
}

// adminDevices adalah bagian devices pada /admin/overview.
type adminDevices struct {
	Registered int64 `json:"registered"`
	Online     int64 `json:"online"`
}

// adminStorage adalah bagian storage pada /admin/overview.
//
// FreeBytes dan UsedPercent bernilai 0 bila kapasitas disk tidak bisa diukur:
// paket ini tidak memegang path filesystem (Config hanya berisi pool database),
// sehingga angka sisa ruang harus datang dari lapisan yang mengetahui
// media root.
type adminStorage struct {
	UsedBytes   int64 `json:"used_bytes"`
	FreeBytes   int64 `json:"free_bytes"`
	UsedPercent int64 `json:"used_percent"`
}

// adminSync adalah bagian sync pada /admin/overview.
type adminSync struct {
	ID     any    `json:"id"`
	Status string `json:"status"`
}

// adminOverview adalah data GET /admin/overview (10.7).
type adminOverview struct {
	Generation any          `json:"generation"`
	Catalog    adminCatalog `json:"catalog"`
	Devices    adminDevices `json:"devices"`
	Storage    adminStorage `json:"storage"`
	Sync       adminSync    `json:"sync"`
}

// handleAdminOverview melayani GET /admin/overview. Route ini hanya terpasang
// bila Config.AdminOnly tidak nil (lihat NewRouter).
func (s *server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	generation, err := s.overviewGeneration(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	catalog, err := s.overviewCatalog(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	devices, err := s.overviewDevices(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	storage, err := s.overviewStorage(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	sync, err := s.overviewSync(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	response.Success(w, adminOverview{
		Generation: generation,
		Catalog:    catalog,
		Devices:    devices,
		Storage:    storage,
		Sync:       sync,
	})
}

// overviewGeneration membaca generation aktif, atau null bila belum ada.
func (s *server) overviewGeneration(ctx context.Context) (any, error) {
	var gen adminGeneration
	err := s.read.QueryRowContext(ctx,
		`SELECT id, label, status, total_bytes FROM generations WHERE status = 'active'`,
	).Scan(&gen.ID, &gen.Label, &gen.Status, &gen.TotalBytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return gen, nil
}

// overviewCatalog menghitung isi katalog.
func (s *server) overviewCatalog(ctx context.Context) (adminCatalog, error) {
	var catalog adminCatalog

	counts := []struct {
		query string
		dest  *int64
	}{
		{`SELECT COUNT(*) FROM titles WHERE is_published = 1`, &catalog.Titles},
		{`SELECT COUNT(*) FROM episodes`, &catalog.Episodes},
		{`SELECT COUNT(*) FROM playables`, &catalog.Playables},
		{`SELECT COUNT(*) FROM renditions WHERE status = 'ready'`, &catalog.RenditionsReady},
	}
	for _, item := range counts {
		if err := s.read.QueryRowContext(ctx, item.query).Scan(item.dest); err != nil {
			return adminCatalog{}, err
		}
	}

	return catalog, nil
}

// overviewDevices menghitung device terdaftar dan device online.
func (s *server) overviewDevices(ctx context.Context) (adminDevices, error) {
	var devices adminDevices
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&devices.Registered); err != nil {
		return adminDevices{}, err
	}

	cutoff := s.clock().Add(-onlineWindow).Format(timeLayout)
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE is_active = 1 AND last_seen_at IS NOT NULL AND last_seen_at >= ?`,
		cutoff,
	).Scan(&devices.Online)
	if err != nil {
		return adminDevices{}, err
	}

	return devices, nil
}

// overviewStorage menjumlahkan total_bytes seluruh generation.
func (s *server) overviewStorage(ctx context.Context) (adminStorage, error) {
	var storage adminStorage
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_bytes), 0) FROM generations`,
	).Scan(&storage.UsedBytes); err != nil {
		return adminStorage{}, err
	}
	return storage, nil
}

// overviewSync membaca sync run terakhir.
func (s *server) overviewSync(ctx context.Context) (adminSync, error) {
	var (
		id     int64
		status string
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, status FROM sync_runs ORDER BY id DESC LIMIT 1`,
	).Scan(&id, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return adminSync{Status: "idle"}, nil
	case err != nil:
		return adminSync{}, err
	}
	return adminSync{ID: id, Status: status}, nil
}
