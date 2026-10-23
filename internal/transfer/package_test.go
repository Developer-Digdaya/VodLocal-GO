package transfer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateManifestRejectsTraversal(t *testing.T) {
	m := &Manifest{
		Format:    Format,
		PackageID: "pkg",
		Items: []Item{{
			Provider: "densgo",
			Category: "movie",
			Title:    TitleData{ExternalID: "movie-1", Kind: "movie", Name: "Film"},
			Files:    []string{"media/a.ts"},
		}},
		Files: []File{{
			Path: "media/../a.ts", Target: "a.ts", Size: 1,
			SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
	if err := ValidateManifest(m); err == nil {
		t.Fatal("manifest dengan path traversal harus ditolak")
	}
}

func TestScanPackagesReportsBrokenPackage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, PackageRootName, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	packages, err := ScanPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0].Error == "" {
		t.Fatalf("packages = %+v, mau satu paket rusak", packages)
	}
}

func TestCleanRelRejectsUnsafePaths(t *testing.T) {
	for _, path := range []string{"../x", "/etc/passwd", `C:\x`, "a/../../b", ""} {
		if _, err := cleanRel(path); err == nil {
			t.Fatalf("cleanRel(%q) harus error", path)
		}
	}
}
