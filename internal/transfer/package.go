// Package transfer implements portable content export/import packages for
// moving media between a Windows download station and Linux bus servers.
//
// A package is a directory containing manifest.json and a media/ tree. The
// manifest uses only logical catalog keys, never local database IDs, so an
// import can create a new generation on the destination without touching
// devices, playback progress, or unrelated catalog rows.
package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Format is the only manifest format understood by this version.
const Format = "vodlocal-export/1"

// ManifestName is the file read/written at the package root.
const ManifestName = "manifest.json"

// PackageRootName is the directory created on the destination drive.
const PackageRootName = "VOD-EXPORT"

// ErrInvalidPackage is returned for a malformed or unsafe package.
var ErrInvalidPackage = errors.New("transfer: paket tidak valid")

// Manifest describes one portable export package.
type Manifest struct {
	Format     string `json:"format"`
	PackageID  string `json:"package_id"`
	CreatedAt  string `json:"created_at"`
	SourceHost string `json:"source_host,omitempty"`
	Items      []Item `json:"items"`
	Files      []File `json:"files"`
	TotalBytes int64  `json:"total_bytes"`
}

// Item is one title in a package. A title may contain a movie or a series.
type Item struct {
	Provider  string     `json:"provider"`
	Category  string     `json:"category"`
	Title     TitleData  `json:"title"`
	Episodes  []Episode  `json:"episodes,omitempty"`
	Playables []Playable `json:"playables"`
	// Files lists package-relative paths (for example
	// "media/dramahits/short/id/ep-1/360p/index.m3u8") owned by this item.
	Files []string `json:"files"`
}

// TitleData is the portable form of a titles row.
type TitleData struct {
	ExternalID      string  `json:"external_id"`
	Kind            string  `json:"kind"`
	Name            string  `json:"title"`
	Slug            string  `json:"slug"`
	Synopsis        string  `json:"synopsis,omitempty"`
	ReleaseYear     int     `json:"release_year,omitempty"`
	ContentRating   string  `json:"content_rating,omitempty"`
	Language        string  `json:"language,omitempty"`
	Country         string  `json:"country,omitempty"`
	DurationSeconds int     `json:"duration_seconds,omitempty"`
	PosterPath      string  `json:"poster_path,omitempty"`
	BackdropPath    string  `json:"backdrop_path,omitempty"`
	Genres          []Genre `json:"genres,omitempty"`
}

// Genre is a portable genre name/slug pair.
type Genre struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Episode is the portable form of an episodes row.
type Episode struct {
	ExternalID      string `json:"external_id"`
	SeasonNumber    int    `json:"season_number"`
	EpisodeNumber   int    `json:"episode_number"`
	Name            string `json:"name,omitempty"`
	Synopsis        string `json:"synopsis,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	AirDate         string `json:"air_date,omitempty"`
	StillPath       string `json:"still_path,omitempty"`
}

// Playable groups renditions/subtitles for one playable. EpisodeExternalID is
// empty for a movie.
type Playable struct {
	EpisodeExternalID string      `json:"episode_external_id,omitempty"`
	DurationSeconds   int         `json:"duration_seconds,omitempty"`
	Encrypted         bool        `json:"encrypted,omitempty"`
	Renditions        []Rendition `json:"renditions"`
	Subtitles         []Subtitle  `json:"subtitles,omitempty"`
}

// Rendition is the portable form of one ready renditions row.
type Rendition struct {
	Quality         string `json:"quality"`
	BitrateKbps     int    `json:"bitrate_kbps,omitempty"`
	Width           int    `json:"width,omitempty"`
	Height          int    `json:"height,omitempty"`
	Container       string `json:"container,omitempty"`
	PlaylistPath    string `json:"playlist_path"`
	SegmentCount    int    `json:"segment_count,omitempty"`
	TotalBytes      int64  `json:"total_bytes,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
}

// Subtitle is the portable form of one subtitle row.
type Subtitle struct {
	Lang     string `json:"lang"`
	Label    string `json:"label,omitempty"`
	Format   string `json:"format"`
	FilePath string `json:"file_path"`
	Bytes    int64  `json:"bytes,omitempty"`
}

// File is one file in the package. Path is relative to the package root;
// Target is relative to the destination generation root.
type File struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PackageInfo is the lightweight view used by the offline picker.
type PackageInfo struct {
	Path       string        `json:"path"`
	PackageID  string        `json:"package_id"`
	CreatedAt  string        `json:"created_at"`
	TotalBytes int64         `json:"total_bytes"`
	Items      []PackageItem `json:"items"`
	Error      string        `json:"error,omitempty"`
}

// PackageItem is one selectable title inside an offline package.
type PackageItem struct {
	Index        int    `json:"index"`
	Provider     string `json:"provider"`
	Category     string `json:"category"`
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	EpisodeCount int    `json:"episode_count"`
	Bytes        int64  `json:"bytes"`
}

// ReadManifest reads and validates manifest.json in packageDir.
func ReadManifest(packageDir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(packageDir, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("%w: baca manifest: %v", ErrInvalidPackage, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest JSON: %v", ErrInvalidPackage, err)
	}
	if err := ValidateManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// WriteManifest writes manifest.json atomically inside packageDir.
func WriteManifest(packageDir string, m *Manifest) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	final := filepath.Join(packageDir, ManifestName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// ValidateManifest checks the package schema and every filesystem path.
func ValidateManifest(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("%w: manifest kosong", ErrInvalidPackage)
	}
	if m.Format != Format {
		return fmt.Errorf("%w: format %q tidak didukung", ErrInvalidPackage, m.Format)
	}
	if strings.TrimSpace(m.PackageID) == "" {
		return fmt.Errorf("%w: package_id kosong", ErrInvalidPackage)
	}
	if len(m.Items) == 0 {
		return fmt.Errorf("%w: tidak berisi konten", ErrInvalidPackage)
	}

	fileByPath := make(map[string]File, len(m.Files))
	for _, file := range m.Files {
		if file.Path == "" {
			return fmt.Errorf("%w: path file kosong", ErrInvalidPackage)
		}
		cleanPath, err := cleanRel(file.Path)
		if err != nil {
			return fmt.Errorf("%w: path file: %v", ErrInvalidPackage, err)
		}
		cleanTarget, err := cleanRel(file.Target)
		if err != nil {
			return fmt.Errorf("%w: target file: %v", ErrInvalidPackage, err)
		}
		if file.Size < 0 {
			return fmt.Errorf("%w: ukuran file negatif", ErrInvalidPackage)
		}
		if !validSHA256(file.SHA256) {
			return fmt.Errorf("%w: sha256 file %q tidak valid", ErrInvalidPackage, file.Path)
		}
		file.Path = cleanPath
		file.Target = cleanTarget
		if _, exists := fileByPath[cleanPath]; exists {
			return fmt.Errorf("%w: file ganda %q", ErrInvalidPackage, cleanPath)
		}
		fileByPath[cleanPath] = file
	}

	total := int64(0)
	targets := make(map[string]struct{})
	for i := range m.Items {
		item := &m.Items[i]
		if strings.TrimSpace(item.Provider) == "" {
			return fmt.Errorf("%w: item %d tanpa provider", ErrInvalidPackage, i+1)
		}
		if item.Title.Kind != "movie" && item.Title.Kind != "series" {
			return fmt.Errorf("%w: item %d kind %q tidak dikenal", ErrInvalidPackage, i+1, item.Title.Kind)
		}
		if strings.TrimSpace(item.Title.ExternalID) == "" {
			return fmt.Errorf("%w: item %d tanpa external_id", ErrInvalidPackage, i+1)
		}
		if len(item.Files) == 0 {
			return fmt.Errorf("%w: item %d tanpa file", ErrInvalidPackage, i+1)
		}
		seenItemFile := make(map[string]struct{}, len(item.Files))
		for _, itemFile := range item.Files {
			clean, err := cleanRel(itemFile)
			if err != nil {
				return fmt.Errorf("%w: item %d file: %v", ErrInvalidPackage, i+1, err)
			}
			file, ok := fileByPath[clean]
			if !ok {
				return fmt.Errorf("%w: item %d merujuk file tidak ada %q", ErrInvalidPackage, i+1, clean)
			}
			if _, dup := seenItemFile[clean]; dup {
				return fmt.Errorf("%w: item %d file ganda %q", ErrInvalidPackage, i+1, clean)
			}
			seenItemFile[clean] = struct{}{}
			if _, dup := targets[file.Target]; dup {
				return fmt.Errorf("%w: target %q dipakai lebih dari satu item", ErrInvalidPackage, file.Target)
			}
			targets[file.Target] = struct{}{}
			total += file.Size
		}
		for _, playable := range item.Playables {
			for _, rendition := range playable.Renditions {
				if _, err := cleanRel(rendition.PlaylistPath); err != nil {
					return fmt.Errorf("%w: playlist item %d: %v", ErrInvalidPackage, i+1, err)
				}
			}
		}
	}
	if m.TotalBytes != 0 && m.TotalBytes != total {
		return fmt.Errorf("%w: total_bytes %d tidak sama dengan jumlah file %d", ErrInvalidPackage, m.TotalBytes, total)
	}
	m.TotalBytes = total
	return nil
}

// ScanPackages scans root/VOD-EXPORT for package directories. A broken package
// is returned with Error set instead of aborting the whole scan.
func ScanPackages(root string) ([]PackageInfo, error) {
	abs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, err
	}
	base := filepath.Join(abs, PackageRootName)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []PackageInfo{}, nil
		}
		return nil, err
	}

	out := make([]PackageInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		info := PackageInfo{Path: dir}
		m, err := ReadManifest(dir)
		if err != nil {
			info.Error = err.Error()
			out = append(out, info)
			continue
		}
		info.PackageID = m.PackageID
		info.CreatedAt = m.CreatedAt
		info.TotalBytes = m.TotalBytes
		for i, item := range m.Items {
			info.Items = append(info.Items, PackageItem{
				Index:        i,
				Provider:     item.Provider,
				Category:     item.Category,
				Title:        item.Title.Name,
				Kind:         item.Title.Kind,
				EpisodeCount: len(item.Episodes),
				Bytes:        itemBytes(m, item),
			})
		}
		out = append(out, info)
	}
	return out, nil
}

func itemBytes(m *Manifest, item Item) int64 {
	byPath := make(map[string]File, len(m.Files))
	for _, f := range m.Files {
		byPath[f.Path] = f
	}
	var total int64
	for _, p := range item.Files {
		total += byPath[p].Size
	}
	return total
}

// cleanRel normalises a manifest path and rejects absolute paths, volume
// names, NUL bytes and traversal. The result always uses forward slashes.
func cleanRel(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", errors.New("path kosong")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("path memuat NUL")
	}
	slash := strings.ReplaceAll(rel, `\`, "/")
	if path.IsAbs(slash) || strings.Contains(slash, ":") {
		return "", fmt.Errorf("path absolut/volume: %q", rel)
	}
	clean := path.Clean(slash)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path keluar paket: %q", rel)
	}
	return clean, nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
