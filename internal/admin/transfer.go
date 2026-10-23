package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"vodlocal/internal/response"
	"vodlocal/internal/transfer"
)

// handleTransferDrives lists writable destinations/sources for export/import.
func (a *Admin) handleTransferDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := a.transfer.Drives()
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca daftar drive.")
		return
	}
	if drives == nil {
		drives = []transfer.Drive{}
	}
	response.Success(w, map[string]any{"drives": drives})
}

// handleTransferPackages scans a selected drive for offline packages.
func (a *Admin) handleTransferPackages(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(r.URL.Query().Get("drive"))
	if !a.validTransferDrive(root, false) {
		response.ErrorMsg(w, response.CodeValidationError, "Drive sumber tidak dikenal.")
		return
	}
	all, err := a.transfer.ListPackages(r.Context(), root)
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca isi HDD.")
		return
	}
	page, perPage := transferPage(r.URL.Query(), 10)
	total := int64(len(all))
	lastPage := pageCount(total, perPage)
	if lastPage > 0 && int64(page) > lastPage {
		page = int(lastPage)
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	packages := all[start:end]
	if packages == nil {
		packages = []transfer.PackageInfo{}
	}
	response.Success(w, map[string]any{
		"packages": packages,
		"meta": map[string]any{
			"page": page, "per_page": perPage, "total": total, "total_pages": lastPage,
		},
	})
}

// handleTransferBatchItems returns every deduplicated item id in a generation
// so the UI can check a whole batch without loading all pages.
func (a *Admin) handleTransferBatchItems(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if label == "" {
		response.ErrorMsg(w, response.CodeValidationError, "Batch tidak dikenal.")
		return
	}
	ids, err := a.transfer.LocalContentIDsByBatch(r.Context(), label)
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca konten batch.")
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	response.Success(w, map[string]any{"item_ids": ids})
}

// handleTransferPackagePoster serves a poster from an offline package.
func (a *Admin) handleTransferPackagePoster(w http.ResponseWriter, r *http.Request) {
	packageDir := strings.TrimSpace(r.URL.Query().Get("package"))
	if !a.validTransferPackage(packageDir) {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("item")))
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	path, err := transfer.PackagePosterPath(packageDir, index)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

// handleTransferLocalContent lists downloaded content available for export.
// Query: page (1-based), per_page (default 30, max 200).
func (a *Admin) handleTransferLocalContent(w http.ResponseWriter, r *http.Request) {
	page, perPage := transferPage(r.URL.Query(), 30)
	items, total, err := a.transfer.ListLocalContent(r.Context(), page, perPage)
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca konten lokal.")
		return
	}
	if items == nil {
		items = []transfer.LocalContent{}
	}
	response.Success(w, map[string]any{
		"items": items,
		"meta": map[string]any{
			"page":        page,
			"per_page":    perPage,
			"total":       total,
			"total_pages": pageCount(total, perPage),
		},
	})
}

func transferPage(query map[string][]string, defaultPerPage int) (int, int) {
	page := 1
	perPage := defaultPerPage
	if raw := strings.TrimSpace(queryGet(query, "page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}
	if raw := strings.TrimSpace(queryGet(query, "per_page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			perPage = n
		}
	}
	if perPage > 200 {
		perPage = 200
	}
	return page, perPage
}

func pageCount(total int64, perPage int) int64 {
	if total <= 0 || perPage <= 0 {
		return 0
	}
	return (total + int64(perPage) - 1) / int64(perPage)
}

// handleTransferExport starts an export to the selected drive.
func (a *Admin) handleTransferExport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderContentMessage(w, r, http.StatusBadRequest, "Formulir export tidak dapat dibaca.")
		return
	}
	destination := strings.TrimSpace(r.PostFormValue("destination"))
	if !a.validTransferDrive(destination, true) {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Drive tujuan export tidak dikenal, belum ter-mount, atau read-only.")
		return
	}

	var ids []int64
	if err := json.Unmarshal([]byte(r.PostFormValue("items")), &ids); err != nil || len(ids) == 0 {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Pilih minimal satu konten untuk diexport.")
		return
	}

	runID, err := a.transfer.StartExport(r.Context(), ids, destination, a.sessionUsername(r))
	if err != nil {
		switch {
		case errors.Is(err, transfer.ErrBusy):
			a.renderContentMessage(w, r, http.StatusConflict,
				"Masih ada proses sync/transfer aktif. Tunggu hingga selesai.")
		case errors.Is(err, transfer.ErrNoContent):
			a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
				"Tidak ada konten yang bisa diexport.")
		case errors.Is(err, transfer.ErrInsufficientSpace):
			a.renderContentMessage(w, r, http.StatusInsufficientStorage,
				"Ruang di drive tujuan tidak mencukupi untuk paket export.")
		default:
			a.log.Warn("admin: mulai export gagal", "err", err, "destination", destination)
			a.renderContentMessage(w, r, http.StatusInternalServerError,
				"Gagal memulai export. Periksa log server.")
		}
		return
	}
	a.flashTo(r, "success", fmt.Sprintf("Export dimulai (run #%d).", runID))
	http.Redirect(w, r, fmt.Sprintf("/sync/%d", runID), http.StatusSeeOther)
}

// handleTransferImport starts an offline import from a package directory on a
// mounted drive.
func (a *Admin) handleTransferImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderContentMessage(w, r, http.StatusBadRequest, "Formulir import tidak dapat dibaca.")
		return
	}
	packageDir := strings.TrimSpace(r.PostFormValue("package"))
	if !a.validTransferPackage(packageDir) {
		a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
			"Paket offline tidak dikenal atau berada di luar folder VOD-EXPORT.")
		return
	}

	var indexes []int
	if raw := strings.TrimSpace(r.PostFormValue("items")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &indexes); err != nil {
			a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
				"Format pilihan konten offline tidak valid.")
			return
		}
	}

	runID, err := a.transfer.StartImport(r.Context(), packageDir, indexes, a.sessionUsername(r))
	if err != nil {
		switch {
		case errors.Is(err, transfer.ErrBusy):
			a.renderContentMessage(w, r, http.StatusConflict,
				"Masih ada proses sync/transfer aktif. Tunggu hingga selesai.")
		case errors.Is(err, transfer.ErrInvalidPackage), errors.Is(err, transfer.ErrNoContent):
			a.renderContentMessage(w, r, http.StatusUnprocessableEntity,
				"Paket offline tidak valid atau rusak.")
		case errors.Is(err, transfer.ErrAlreadyExists):
			a.renderContentMessage(w, r, http.StatusConflict,
				"Konten sudah ada di server ini; import dibatalkan agar tidak duplikat.")
		case errors.Is(err, transfer.ErrInsufficientSpace):
			a.renderContentMessage(w, r, http.StatusInsufficientStorage,
				"Ruang penyimpanan internal tidak mencukupi untuk import.")
		default:
			a.log.Warn("admin: mulai import gagal", "err", err, "package", packageDir)
			a.renderContentMessage(w, r, http.StatusInternalServerError,
				"Gagal memulai import. Periksa log server.")
		}
		return
	}
	a.flashTo(r, "success", fmt.Sprintf("Import dimulai (run #%d).", runID))
	http.Redirect(w, r, fmt.Sprintf("/sync/%d", runID), http.StatusSeeOther)
}

func (a *Admin) validTransferDrive(path string, requireWritable bool) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	drives, err := a.transfer.Drives()
	if err != nil {
		return false
	}
	for _, drive := range drives {
		if samePath(drive.Path, path) && (!requireWritable || drive.Writable) {
			return true
		}
	}
	return false
}

func (a *Admin) validTransferPackage(packageDir string) bool {
	packageDir = strings.TrimSpace(packageDir)
	if packageDir == "" {
		return false
	}
	abs, err := filepath.Abs(packageDir)
	if err != nil {
		return false
	}
	drives, err := a.transfer.Drives()
	if err != nil {
		return false
	}
	for _, drive := range drives {
		base, err := filepath.Abs(filepath.Join(drive.Path, transfer.PackageRootName))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(base, abs)
		if err != nil || rel == "." || filepath.IsAbs(rel) {
			continue
		}
		if !strings.ContainsAny(rel, `/\`) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	// Windows drive letters are case-insensitive.
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
