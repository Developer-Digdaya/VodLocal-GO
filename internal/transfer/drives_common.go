package transfer

import (
	"os"
	"path/filepath"
)

// Drive is a destination that can receive an export or contain packages.
type Drive struct {
	Path       string `json:"path"`
	Label      string `json:"label,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	Removable  bool   `json:"removable"`
	Writable   bool   `json:"writable"`
	Packages   int    `json:"packages"`
}

// countPackages returns the number of plausible package directories under
// driveRoot/VOD-EXPORT.
func countPackages(driveRoot string) int {
	entries, err := os.ReadDir(filepath.Join(driveRoot, PackageRootName))
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() && !hasPrefix(entry.Name(), ".tmp-") {
			n++
		}
	}
	return n
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
