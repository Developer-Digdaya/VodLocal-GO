package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPackage membuat satu paket valid berisi satu item di bawah
// <root>/VOD-EXPORT/<packageID>.
func writeTestPackage(t *testing.T, root, packageID, provider, externalID string) {
	t.Helper()

	dir := filepath.Join(root, PackageRootName, packageID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir paket: %v", err)
	}

	target := "densgo/movie/" + externalID + "/360p/index.m3u8"
	manifest := &Manifest{
		Format:    Format,
		PackageID: packageID,
		Items: []Item{{
			Provider: provider,
			Category: "movie",
			Title: TitleData{
				ExternalID: externalID,
				Kind:       "movie",
				Name:       "Film " + externalID,
			},
			Files: []string{"media/" + target},
		}},
		Files: []File{{
			Path:   "media/" + target,
			Target: target,
			Size:   10,
			SHA256: strings.Repeat("a", 64),
		}},
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatalf("tulis manifest: %v", err)
	}
}

func TestExportedKeys(t *testing.T) {
	root := t.TempDir()
	writeTestPackage(t, root, "VOD-EXPORT-1", "densgo", "mv-1")
	writeTestPackage(t, root, "VOD-EXPORT-2", "dramahits", "sr-1")
	// Staging harus dilewati.
	writeTestPackage(t, root, ".tmp-VOD-EXPORT-3", "densgo", "mv-3")

	// Paket rusak harus dilewati, bukan menggagalkan seluruh scan.
	badDir := filepath.Join(root, PackageRootName, "VOD-EXPORT-BAD")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, ManifestName), []byte("{rusak"), 0o644); err != nil {
		t.Fatal(err)
	}

	keys, err := ExportedKeys(root)
	if err != nil {
		t.Fatalf("ExportedKeys() error = %v", err)
	}
	if got := len(keys); got != 2 {
		t.Fatalf("jumlah key = %d, mau 2 (keys=%v)", got, keys)
	}
	if got := keys[exportKey("densgo", "mv-1")]; got != "VOD-EXPORT-1" {
		t.Errorf("paket mv-1 = %q, mau VOD-EXPORT-1", got)
	}
	if got := keys[exportKey("dramahits", "sr-1")]; got != "VOD-EXPORT-2" {
		t.Errorf("paket sr-1 = %q, mau VOD-EXPORT-2", got)
	}
	if _, ok := keys[exportKey("densgo", "mv-3")]; ok {
		t.Error("paket staging .tmp- ikut terdaftar")
	}
}

func TestExportedKeysWithoutPackages(t *testing.T) {
	keys, err := ExportedKeys(t.TempDir())
	if err != nil {
		t.Fatalf("ExportedKeys() error = %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, mau kosong", keys)
	}
}

func TestMarkExported(t *testing.T) {
	items := []LocalContent{
		{Provider: "densgo", ExternalID: "mv-1", Title: "Ada"},
		{Provider: "densgo", ExternalID: "mv-2", Title: "Belum"},
		{Provider: "dramahits", ExternalID: "mv-1", Title: "Provider lain"},
	}
	keys := map[string]string{exportKey("densgo", "mv-1"): "VOD-EXPORT-9"}

	MarkExported(items, keys)

	if !items[0].AlreadyExported || items[0].ExportedPackage != "VOD-EXPORT-9" {
		t.Fatalf("item 0 = %+v, mau already_exported dengan paket VOD-EXPORT-9", items[0])
	}
	if items[1].AlreadyExported || items[1].ExportedPackage != "" {
		t.Fatalf("item 1 = %+v, mau tidak ditandai", items[1])
	}
	if items[2].AlreadyExported {
		t.Fatalf("item 2 = %+v, provider berbeda tidak boleh cocok", items[2])
	}
}
