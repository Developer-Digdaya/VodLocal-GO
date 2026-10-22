package admin

import (
	"context"
	"net/http"
)

// catalogStats adalah ringkasan katalog pada dashboard (§10.2).
type catalogStats struct {
	Titles          int64
	Episodes        int64
	Playables       int64
	RenditionsReady int64
}

// deviceStats adalah ringkasan perangkat pada dashboard (§10.2).
type deviceStats struct {
	Registered int64
	Online     int64
}

// storageStats adalah ringkasan penyimpanan pada dashboard (§10.2).
type storageStats struct {
	UsedBytes   int64
	TotalBytes  int64
	Percent     int
	HasPercent  bool
	WarnPercent int
	Warn        bool
}

// dashboardData adalah data halaman dashboard.
type dashboardData struct {
	base
	Generation       *generationView
	Catalog          catalogStats
	Devices          deviceStats
	Storage          storageStats
	LastSync         *syncRunView
	ActiveSync       *syncRunView
	ActiveSyncStatus syncStatusView
}

// handleDashboard melayani GET /.
func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	generation, err := a.activeGenerationView(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca versi media aktif.")
		return
	}

	catalog, err := a.catalogStats(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung isi katalog.")
		return
	}

	devices, err := a.deviceStats(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung perangkat.")
		return
	}

	storage, err := a.storageStatsView(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal menghitung pemakaian penyimpanan.")
		return
	}

	lastSync, err := a.latestSyncRun(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca riwayat sinkronisasi.")
		return
	}

	activeSync, err := a.activeSyncRun(ctx)
	if err != nil {
		a.internalError(w, r, err, "Gagal membaca sinkronisasi yang berjalan.")
		return
	}

	data := dashboardData{
		base:       a.base(w, r, "Dashboard", "dashboard"),
		Generation: generation,
		Catalog:    catalog,
		Devices:    devices,
		Storage:    storage,
		LastSync:   lastSync,
		ActiveSync: activeSync,
	}
	if activeSync != nil {
		data.ActiveSyncStatus = a.syncStatusView(activeSync)
	}
	a.renderPage(w, r, "dashboard", data, http.StatusOK)
}

// catalogStats menghitung judul, episode, playable, dan rendition siap.
//
// Rendition dihitung lintas batch: selama masih ada rendition ready, konten
// tetap bisa diputar walau batch tersebut bukan batch aktif.
func (a *Admin) catalogStats(ctx context.Context) (catalogStats, error) {
	var stats catalogStats

	counts := []struct {
		query string
		args  []any
		dest  *int64
	}{
		{`SELECT COUNT(*) FROM titles`, nil, &stats.Titles},
		{`SELECT COUNT(*) FROM episodes`, nil, &stats.Episodes},
		{`SELECT COUNT(*) FROM playables`, nil, &stats.Playables},
		{`SELECT COUNT(*) FROM renditions WHERE status = 'ready'`,
			nil, &stats.RenditionsReady},
	}

	for _, item := range counts {
		if err := a.read.QueryRowContext(ctx, item.query, item.args...).Scan(item.dest); err != nil {
			return catalogStats{}, err
		}
	}
	return stats, nil
}

// deviceStats menghitung perangkat terdaftar dan perangkat online (§10.4).
func (a *Admin) deviceStats(ctx context.Context) (deviceStats, error) {
	var stats deviceStats
	if err := a.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&stats.Registered); err != nil {
		return deviceStats{}, err
	}

	cutoff := a.nowUTC().Add(-onlineWindow).Format(timeLayout)
	err := a.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices
		 WHERE is_active = 1 AND last_seen_at IS NOT NULL AND last_seen_at >= ?`,
		cutoff,
	).Scan(&stats.Online)
	if err != nil {
		return deviceStats{}, err
	}
	return stats, nil
}

// storageStatsView menghitung pemakaian penyimpanan beserta peringatan.
func (a *Admin) storageStatsView(ctx context.Context) (storageStats, error) {
	var stats storageStats

	if err := a.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_bytes), 0) FROM generations`,
	).Scan(&stats.UsedBytes); err != nil {
		return storageStats{}, err
	}

	if a.usage != nil {
		used, capacity, err := a.usage()
		switch {
		case err != nil:
			a.log.Warn("admin: gagal mengukur pemakaian disk", "err", err)
		case capacity > 0:
			stats.UsedBytes = used
			stats.TotalBytes = capacity
			stats.Percent = int(used * 100 / capacity)
			stats.HasPercent = true
		}
	}

	stats.WarnPercent = a.settingInt(ctx, settingStorageWarnPercent)
	stats.Warn = stats.HasPercent && stats.Percent > stats.WarnPercent
	return stats, nil
}
