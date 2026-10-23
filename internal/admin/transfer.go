package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
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
	packages, err := a.transfer.ListPackages(root)
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca isi HDD.")
		return
	}
	if packages == nil {
		packages = []transfer.PackageInfo{}
	}
	response.Success(w, map[string]any{"packages": packages})
}

// handleTransferLocalContent lists downloaded content available for export.
func (a *Admin) handleTransferLocalContent(w http.ResponseWriter, r *http.Request) {
	items, err := a.transfer.ListLocalContent(r.Context())
	if err != nil {
		response.ErrorMsg(w, response.CodeInternalError, "Gagal membaca konten lokal.")
		return
	}
	if items == nil {
		items = []transfer.LocalContent{}
	}
	response.Success(w, map[string]any{"items": items})
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
