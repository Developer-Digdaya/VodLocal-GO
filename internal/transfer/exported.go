package transfer

import "path/filepath"

// exportKey identifies one portable title inside a package: provider code plus
// the provider's external id. This is the logical identity used by the export
// and import manifests, so it is portable across servers and drives.
func exportKey(provider, externalID string) string {
	return provider + "\x00" + externalID
}

// MarkExported sets AlreadyExported and ExportedPackage on items that are
// already present in a package on the destination drive.
func MarkExported(items []LocalContent, keys map[string]string) {
	for i := range items {
		if pkg, ok := keys[exportKey(items[i].Provider, items[i].ExternalID)]; ok {
			items[i].AlreadyExported = true
			items[i].ExportedPackage = pkg
		}
	}
}

// ExportedKeys reads VOD-EXPORT/*/manifest.json on driveRoot and maps every
// title already present in a package to the package id that contains it.
//
// Invalid manifests and staging directories (.tmp-*) are skipped. A missing or
// empty VOD-EXPORT directory yields an empty map, not an error.
func ExportedKeys(driveRoot string) (map[string]string, error) {
	packages, err := ScanPackages(driveRoot)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]string)
	for _, pkg := range packages {
		if pkg.Error != "" {
			continue
		}
		id := pkg.PackageID
		if id == "" {
			id = filepath.Base(pkg.Path)
		}
		for _, item := range pkg.Items {
			key := exportKey(item.Provider, item.ExternalID)
			if _, exists := keys[key]; !exists {
				keys[key] = id
			}
		}
	}
	return keys, nil
}
