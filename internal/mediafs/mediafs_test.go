package mediafs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newManager returns a Manager rooted in a fresh, nested temp directory. The
// extra "media" element proves that New creates missing parents.
func newManager(t *testing.T) *Manager {
	t.Helper()
	root := filepath.Join(t.TempDir(), "media")
	m, err := New(root)
	if err != nil {
		t.Fatalf("New(%q) = %v, want nil error", root, err)
	}
	if got := m.Root(); got != root {
		t.Fatalf("Root() = %q, want %q", got, root)
	}
	info, err := os.Stat(m.Root())
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("root %q is not a directory", m.Root())
	}
	return m
}

// writeInto writes body to name inside dir and returns the file path.
func writeInto(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) = %v, want nil error", p, err)
	}
	return p
}

// mustExist fails the test when p is missing.
func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %q = %v, want the path to exist", p, err)
	}
}

// mustNotExist fails the test when anything is present at p.
func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %q = %v, want the path to be absent", p, err)
	}
}

// 1. New + NewStaging create .building-<label> and Dir() points at it.
func TestNewStagingCreatesStagingDir(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	want := filepath.Join(m.Root(), ".building-gen-000001")
	if got := b.Dir(); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if b.Label() != "gen-000001" {
		t.Errorf("Label() = %q, want %q", b.Label(), "gen-000001")
	}
	if b.Closed() {
		t.Error("Closed() = true on a fresh build, want false")
	}
	info, err := os.Stat(b.Dir())
	if err != nil {
		t.Fatalf("stat staging dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("staging %q is not a directory", b.Dir())
	}
	// The final generation must not exist before the build is committed.
	if m.Exists("gen-000001") {
		t.Error("Exists() = true before commit, want false")
	}
}

// A stale staging directory from an interrupted attempt is replaced, contents
// and all, by NewStaging.
func TestNewStagingReplacesStaleStaging(t *testing.T) {
	m := newManager(t)

	first, err := m.NewStaging("gen-000002")
	if err != nil {
		t.Fatalf("first NewStaging = %v, want nil error", err)
	}
	stale := writeInto(t, first.Dir(), "stale.ts", "leftover")

	second, err := m.NewStaging("gen-000002")
	if err != nil {
		t.Fatalf("second NewStaging = %v, want nil error", err)
	}
	if second.Dir() != first.Dir() {
		t.Fatalf("Dir() = %q, want %q", second.Dir(), first.Dir())
	}
	mustNotExist(t, stale)
}

// 2. Commit renames the staging dir to the generation dir and closes the build.
func TestCommitPublishesGeneration(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	// Writing inside Dir() must work on Windows too, so exercise it for real.
	writeInto(t, b.Dir(), "index.m3u8", "#EXTM3U\n")
	if err := os.MkdirAll(filepath.Join(b.Dir(), "seg"), 0o755); err != nil {
		t.Fatalf("MkdirAll inside staging: %v", err)
	}
	writeInto(t, filepath.Join(b.Dir(), "seg"), "0.ts", "segment")

	staging := b.Dir()
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}
	if !b.Closed() {
		t.Error("Closed() = false after Commit, want true")
	}
	mustNotExist(t, staging)

	final := filepath.Join(m.Root(), "gen-000001")
	mustExist(t, final)
	if !m.Exists("gen-000001") {
		t.Error("Exists() = false after commit, want true")
	}
	for _, name := range []string{"index.m3u8", filepath.Join("seg", "0.ts")} {
		mustExist(t, filepath.Join(final, name))
	}
	body, err := os.ReadFile(filepath.Join(final, "index.m3u8"))
	if err != nil {
		t.Fatalf("ReadFile = %v, want nil error", err)
	}
	if string(body) != "#EXTM3U\n" {
		t.Errorf("published content = %q, want %q", body, "#EXTM3U\n")
	}
}

// 3. A second Commit fails with ErrClosed and leaves the generation intact.
func TestCommitTwiceReturnsErrClosed(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("first Commit() = %v, want nil error", err)
	}
	if err := b.Commit(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Commit() = %v, want ErrClosed", err)
	}
	if !m.Exists("gen-000001") {
		t.Error("Exists() = false after failed second commit, want true")
	}
}

// Commit refuses to overwrite an existing generation and keeps the build open.
func TestCommitRefusesExistingGeneration(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000003")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	// Someone else publishes the generation between NewStaging and Commit.
	final := filepath.Join(m.Root(), "gen-000003")
	if err := os.MkdirAll(final, 0o755); err != nil {
		t.Fatalf("MkdirAll final: %v", err)
	}

	if err := b.Commit(); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Commit() = %v, want ErrAlreadyExists", err)
	}
	if b.Closed() {
		t.Error("Closed() = true after failed Commit, want false")
	}
	mustExist(t, b.Dir())

	// The build is still usable, so it can be released with Abort.
	if err := b.Abort(); err != nil {
		t.Fatalf("Abort() after failed Commit = %v, want nil error", err)
	}
	if !b.Closed() {
		t.Error("Closed() = false after Abort, want true")
	}
}

// 4. Abort throws the staging directory away.
func TestAbortRemovesStaging(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	writeInto(t, b.Dir(), "index.m3u8", "#EXTM3U\n")
	staging := b.Dir()

	if err := b.Abort(); err != nil {
		t.Fatalf("Abort() = %v, want nil error", err)
	}
	if !b.Closed() {
		t.Error("Closed() = false after Abort, want true")
	}
	mustNotExist(t, staging)
	if m.Exists("gen-000001") {
		t.Error("Exists() = true after Abort, want false")
	}
	if err := b.Abort(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Abort() = %v, want ErrClosed", err)
	}
	if err := b.Commit(); !errors.Is(err, ErrClosed) {
		t.Errorf("Commit() after Abort = %v, want ErrClosed", err)
	}
}

// Abort after a successful Commit must not delete the published generation.
func TestAbortAfterCommitReturnsErrClosed(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}
	if err := b.Abort(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Abort() after Commit = %v, want ErrClosed", err)
	}
	if !m.Exists("gen-000001") {
		t.Error("Exists() = false after Abort of a committed build, want true")
	}
}

// 5. Remove deletes a published generation; a second Remove reports ErrNotFound.
func TestRemoveGeneration(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	writeInto(t, b.Dir(), "index.m3u8", "#EXTM3U\n")
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}
	final, err := m.GenerationDir("gen-000001")
	if err != nil {
		t.Fatalf("GenerationDir = %v, want nil error", err)
	}
	mustExist(t, final)

	if err := m.Remove("gen-000001"); err != nil {
		t.Fatalf("Remove() = %v, want nil error", err)
	}
	mustNotExist(t, final)
	if m.Exists("gen-000001") {
		t.Error("Exists() = true after Remove, want false")
	}
	if err := m.Remove("gen-000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Remove() = %v, want ErrNotFound", err)
	}
}

// Remove rejects malformed labels before touching the filesystem.
func TestRemoveInvalidLabel(t *testing.T) {
	m := newManager(t)

	for _, label := range []string{"gen-1", "../x", "gen-000001/../x", ""} {
		if err := m.Remove(label); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidLabel", label, err)
		}
	}
}

// 6. Invalid labels are rejected by every entry point.
func TestInvalidLabels(t *testing.T) {
	m := newManager(t)

	for _, label := range []string{"gen-1", "../x", "gen-000001/../x"} {
		t.Run(label, func(t *testing.T) {
			if _, err := m.GenerationDir(label); !errors.Is(err, ErrInvalidLabel) {
				t.Errorf("GenerationDir(%q) = %v, want ErrInvalidLabel", label, err)
			}
			if _, err := m.NewStaging(label); !errors.Is(err, ErrInvalidLabel) {
				t.Errorf("NewStaging(%q) = %v, want ErrInvalidLabel", label, err)
			}
			if _, err := m.Resolve(label, "index.m3u8"); !errors.Is(err, ErrInvalidLabel) {
				t.Errorf("Resolve(%q, ...) = %v, want ErrInvalidLabel", label, err)
			}
			if m.Exists(label) {
				t.Errorf("Exists(%q) = true, want false", label)
			}
		})
	}
}

// 7. Resolve returns an absolute path inside the generation directory.
func TestResolveValidPaths(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	if err := os.MkdirAll(filepath.Join(b.Dir(), "seg"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeInto(t, filepath.Join(b.Dir(), "seg"), "0.ts", "segment")
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}

	base, err := m.GenerationDir("gen-000001")
	if err != nil {
		t.Fatalf("GenerationDir = %v, want nil error", err)
	}

	cases := map[string]string{
		"index.m3u8":            filepath.Join(base, "index.m3u8"),
		"seg/0.ts":              filepath.Join(base, "seg", "0.ts"),
		`seg\0.ts`:              filepath.Join(base, "seg", "0.ts"),
		"./seg/./0.ts":          filepath.Join(base, "seg", "0.ts"),
		"seg//0.ts":             filepath.Join(base, "seg", "0.ts"),
		"nested/dir/file.m4s":   filepath.Join(base, "nested", "dir", "file.m4s"),
		"seg/../index.m3u8":     filepath.Join(base, "index.m3u8"),
		"dot.file.with.many.ts": filepath.Join(base, "dot.file.with.many.ts"),
	}
	for rel, want := range cases {
		got, err := m.Resolve("gen-000001", rel)
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want nil error", rel, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", rel, got, want)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("Resolve(%q) = %q, want an absolute path", rel, got)
		}
		if !strings.HasPrefix(got, base+string(filepath.Separator)) {
			t.Errorf("Resolve(%q) = %q, want it to start with %q", rel, got, base)
		}
	}

	// The resolved path must be usable for real I/O on Windows.
	segPath, err := m.Resolve("gen-000001", "seg/0.ts")
	if err != nil {
		t.Fatalf("Resolve = %v, want nil error", err)
	}
	body, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v, want nil error", segPath, err)
	}
	if string(body) != "segment" {
		t.Errorf("content = %q, want %q", body, "segment")
	}
}

// 8. Resolve rejects traversal attempts.
func TestResolveRejectsTraversal(t *testing.T) {
	m := newManager(t)

	bad := []string{
		"../x",
		"a/../../b",
		"/etc/passwd",
		`..\x`,
		"",
		"..",
		".",
		"./..",
		"seg/../../x",
		`seg\..\..\x`,
		"../../x",
		`C:\windows\win.ini`,
		"//server/share/x",
		`\\server\share\x`,
		"a/..",
		"a/b/../../..",
		"index.m3u8:stream",
	}
	for _, rel := range bad {
		got, err := m.Resolve("gen-000001", rel)
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) = (%q, %v), want ErrInvalidPath", rel, got, err)
			continue
		}
		if got != "" {
			t.Errorf("Resolve(%q) returned path %q on error, want empty", rel, got)
		}
	}
}

// 9. NewStaging refuses to start when the generation already exists.
func TestNewStagingWhenGenerationExists(t *testing.T) {
	m := newManager(t)

	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}

	if _, err := m.NewStaging("gen-000001"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("NewStaging on an existing generation = %v, want ErrAlreadyExists", err)
	}
	if !m.Exists("gen-000001") {
		t.Error("Exists() = false after refused NewStaging, want true")
	}
	if _, err := os.Stat(filepath.Join(m.Root(), ".building-gen-000001")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging exists after refused NewStaging (%v), want it absent", err)
	}
}

// 10. Exists tracks the generation directory across the build lifecycle.
func TestExists(t *testing.T) {
	m := newManager(t)

	if m.Exists("gen-000001") {
		t.Fatal("Exists() = true before any build, want false")
	}
	b, err := m.NewStaging("gen-000001")
	if err != nil {
		t.Fatalf("NewStaging = %v, want nil error", err)
	}
	if m.Exists("gen-000001") {
		t.Error("Exists() = true while staging only, want false")
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit() = %v, want nil error", err)
	}
	if !m.Exists("gen-000001") {
		t.Error("Exists() = false after commit, want true")
	}
	if err := m.Remove("gen-000001"); err != nil {
		t.Fatalf("Remove() = %v, want nil error", err)
	}
	if m.Exists("gen-000001") {
		t.Error("Exists() = true after Remove, want false")
	}
}

// New rejects an empty root and keeps relative roots absolute.
func TestNewRoot(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") = nil error, want an error")
	}
	if _, err := New("   "); err == nil {
		t.Error("New(\"   \") = nil error, want an error")
	}

	dir := t.TempDir()
	relative, err := filepath.Rel(mustGetwd(t), filepath.Join(dir, "media"))
	if err != nil {
		t.Skipf("cannot build a relative path on this volume: %v", err)
	}
	m, err := New(relative)
	if err != nil {
		t.Fatalf("New(%q) = %v, want nil error", relative, err)
	}
	if !filepath.IsAbs(m.Root()) {
		t.Errorf("Root() = %q, want an absolute path", m.Root())
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "..")); err != nil {
		t.Fatalf("stat parent of root: %v", err)
	}
	if info, err := os.Stat(m.Root()); err != nil || !info.IsDir() {
		t.Fatalf("root %q missing or not a directory", m.Root())
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return wd
}
