// Package mediafs manages generations of directories that hold generated media
// files (segments, playlists, thumbnails, ...).
//
// A Manager owns one root directory and one sub-directory per generation. A
// generation is never written in place: the caller asks for a staging directory
// with NewStaging, fills it, and publishes it under its final name with
// Build.Commit. Commit is a single os.Rename, so a concurrent reader observes
// either the previous generation or the complete new one, never a directory
// that is still being filled.
//
// On-disk layout:
//
//	<root>/<label>/            published generation
//	<root>/.building-<label>/  staging area of an in-progress build
//
// Labels must match ^gen-[0-9]{6}$ (the literal prefix "gen-", a dash and
// exactly six decimal digits), so a label taken from an untrusted source can
// never escape the root directory. Relative paths handed to Resolve are
// validated against directory traversal in the same spirit.
//
// A Manager and its Builds are not safe for concurrent use by multiple
// goroutines; values returned by NewStaging belong to exactly one caller.
package mediafs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Sentinel errors returned by this package. They are always wrapped with
// additional context, so callers should test them with errors.Is.
var (
	// ErrInvalidLabel is returned when a generation label does not match
	// ^gen-[0-9]{6}$.
	ErrInvalidLabel = errors.New("mediafs: invalid label")
	// ErrInvalidPath is returned when a relative path is empty, absolute, or
	// would resolve outside of its generation directory.
	ErrInvalidPath = errors.New("mediafs: invalid relative path")
	// ErrAlreadyExists is returned when the final generation directory is
	// already present.
	ErrAlreadyExists = errors.New("mediafs: generation already exists")
	// ErrNotFound is returned when the requested generation directory does not
	// exist.
	ErrNotFound = errors.New("mediafs: generation not found")
	// ErrClosed is returned when a Build is used after Commit or Abort.
	ErrClosed = errors.New("mediafs: build already closed")
)

const (
	// stagingPrefix is prepended to a label to name its staging directory.
	stagingPrefix = ".building-"
	// dirMode is the permission used for every directory this package creates.
	dirMode os.FileMode = 0o755
)

// labelPattern is the single source of truth for valid generation labels.
var labelPattern = regexp.MustCompile(`^gen-[0-9]{6}$`)

// Manager hands out and removes generation directories below a single root.
type Manager struct {
	root string
}

// New prepares root for use as the media root and returns a Manager for it.
//
// The root is created (including missing parents) with mode 0755 when it does
// not exist yet, and its absolute path is retained, so the paths returned by
// the Manager do not depend on the process working directory.
func New(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("mediafs: empty root path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("mediafs: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, dirMode); err != nil {
		return nil, fmt.Errorf("mediafs: create root %q: %w", abs, err)
	}
	return &Manager{root: abs}, nil
}

// Root returns the absolute path of the media root.
func (m *Manager) Root() string {
	return m.root
}

// GenerationDir validates label and returns the absolute path of its published
// generation directory. The directory is not created and does not have to
// exist.
func (m *Manager) GenerationDir(label string) (string, error) {
	if err := validateLabel(label); err != nil {
		return "", err
	}
	return filepath.Join(m.root, label), nil
}

// NewStaging starts a new build for label and returns it.
//
// A staging directory left over from an earlier, interrupted attempt is
// removed first, then recreated with mode 0755. If the final generation
// directory already exists the build is refused with ErrAlreadyExists; the
// caller has to Remove the generation first.
func (m *Manager) NewStaging(label string) (*Build, error) {
	if err := validateLabel(label); err != nil {
		return nil, err
	}
	final := filepath.Join(m.root, label)
	exists, err := pathExists(final)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, label)
	}
	staging := m.stagingDir(label)
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("mediafs: remove stale staging %q: %w", staging, err)
	}
	if err := os.MkdirAll(staging, dirMode); err != nil {
		return nil, fmt.Errorf("mediafs: create staging %q: %w", staging, err)
	}
	return &Build{
		manager: m,
		label:   label,
		staging: staging,
		final:   final,
	}, nil
}

// Remove deletes the published generation label and everything below it.
//
// It returns ErrInvalidLabel for a malformed label and ErrNotFound when the
// generation directory is absent, so removing an unknown generation twice is
// reported rather than silently ignored.
func (m *Manager) Remove(label string) error {
	if err := validateLabel(label); err != nil {
		return err
	}
	dir := filepath.Join(m.root, label)
	exists, err := pathExists(dir)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, label)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("mediafs: remove generation %q: %w", dir, err)
	}
	return nil
}

// Exists reports whether the published generation directory for label is
// present. It returns false for a malformed label and for a file that happens
// to carry the generation's name.
func (m *Manager) Exists(label string) bool {
	if err := validateLabel(label); err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(m.root, label))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Resolve maps the relative path rel inside the published generation label to
// an absolute filesystem path.
//
// rel must be relative, non-empty, and must stay inside the generation
// directory: absolute paths, volume names, ".." elements and any other form
// that filepath.Clean could turn into an escape are rejected with
// ErrInvalidPath. Both separators are understood, so a Windows-style
// `..\x` is rejected on every platform. The returned path is not required to
// exist.
func (m *Manager) Resolve(label, rel string) (string, error) {
	if err := validateLabel(label); err != nil {
		return "", err
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	base := filepath.Join(m.root, label)
	full := filepath.Join(base, clean)
	if !isWithin(base, full) {
		return "", fmt.Errorf("%w: %q escapes %s", ErrInvalidPath, rel, label)
	}
	return full, nil
}

// stagingDir returns the staging directory of label without validating it.
func (m *Manager) stagingDir(label string) string {
	return filepath.Join(m.root, stagingPrefix+label)
}

// Build is one in-progress generation: a staging directory that is promoted to
// its final name by Commit or thrown away by Abort. Exactly one of the two ends
// the build; every later call returns ErrClosed.
type Build struct {
	manager *Manager
	label   string
	staging string
	final   string
	closed  bool
}

// Label returns the generation label this build will publish.
func (b *Build) Label() string {
	return b.label
}

// Dir returns the absolute path of the staging directory, which is where the
// caller writes the files of this generation. After a successful Commit the
// staging directory has been renamed to the generation directory, so the
// returned path no longer exists; read the files through Manager.Resolve (or
// Manager.GenerationDir) at that point.
func (b *Build) Dir() string {
	return b.staging
}

// Commit publishes the build by renaming the staging directory to its final
// generation name in one atomic step.
//
// If the final directory already exists the commit fails with
// ErrAlreadyExists and the staging directory is left untouched, so the caller
// can still Abort. The build is only closed on success; a failed commit may be
// retried. Aborting is the only way to release a build that cannot be
// committed.
func (b *Build) Commit() error {
	if b.closed {
		return fmt.Errorf("%w: commit of %s", ErrClosed, b.label)
	}
	// os.Rename replaces the target on POSIX but fails on Windows, so check
	// explicitly to get one consistent, documented error on every platform.
	exists, err := pathExists(b.final)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, b.label)
	}
	if err := os.Rename(b.staging, b.final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, b.label)
		}
		return fmt.Errorf("mediafs: commit %s: %w", b.label, err)
	}
	b.closed = true
	return nil
}

// Abort throws the staging directory away.
//
// A successful Abort closes the build; calling Abort (or Commit) afterwards
// returns ErrClosed, and an aborted build never appears as a generation. If the
// removal itself fails the error is returned and the build stays open, so Abort
// can be retried.
func (b *Build) Abort() error {
	if b.closed {
		return fmt.Errorf("%w: abort of %s", ErrClosed, b.label)
	}
	if err := os.RemoveAll(b.staging); err != nil {
		return fmt.Errorf("mediafs: abort %s: %w", b.label, err)
	}
	b.closed = true
	return nil
}

// Closed reports whether the build has been committed or aborted.
func (b *Build) Closed() bool {
	return b.closed
}

// validateLabel rejects anything that is not exactly gen- plus six digits.
func validateLabel(label string) error {
	if !labelPattern.MatchString(label) {
		return fmt.Errorf("%w: %q", ErrInvalidLabel, label)
	}
	return nil
}

// cleanRel normalises a caller-supplied relative path to the separator of the
// running system, rejecting everything that could leave the generation
// directory. It works on slash-separated paths after converting backslashes, so
// the verdict is the same on Windows and on POSIX.
func cleanRel(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q contains a NUL byte", ErrInvalidPath, rel)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalidPath, rel)
	}
	// Treat both separators as separators; strings.ReplaceAll keeps a leading
	// "//" or "\\", which path.IsAbs below still recognises as absolute.
	slash := strings.ReplaceAll(rel, `\`, "/")
	if path.IsAbs(slash) {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalidPath, rel)
	}
	// A colon can only be a Windows volume name or an NTFS alternate data
	// stream here, never part of a media file name we want to allow.
	if strings.Contains(slash, ":") {
		return "", fmt.Errorf("%w: %q contains a volume or stream separator", ErrInvalidPath, rel)
	}
	clean := path.Clean(slash)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q escapes the generation directory", ErrInvalidPath, rel)
	}
	return filepath.FromSlash(clean), nil
}

// isWithin reports whether target stays inside base after cleaning.
func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pathExists reports whether anything (file or directory) exists at p.
func pathExists(p string) (bool, error) {
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("mediafs: stat %q: %w", p, err)
	}
	return true, nil
}
