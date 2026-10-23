package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// copyAndHash copies src to dst while hashing it. The destination is replaced
// atomically only after the copy succeeds.
func copyAndHash(src, dst string) (FileInfo, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return FileInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return FileInfo{}, fmt.Errorf("bukan file biasa/simlink: %s", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return FileInfo{}, err
	}

	in, err := os.Open(src)
	if err != nil {
		return FileInfo{}, err
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return FileInfo{}, err
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil {
		copyErr = syncFile(tmp)
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return FileInfo{}, copyErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return FileInfo{}, err
	}
	return FileInfo{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// verifyFile checks an existing package file against the manifest.
func verifyFile(src string, want File) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("bukan file biasa/simlink: %s", src)
	}
	if info.Size() != want.Size {
		return fmt.Errorf("ukuran %s = %d, mau %d", src, info.Size(), want.Size)
	}
	got, _, err := hashFile(src)
	if err != nil {
		return err
	}
	if got != want.SHA256 {
		return fmt.Errorf("checksum %s tidak cocok", src)
	}
	return nil
}

// hashFile returns the SHA-256 and size of path.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// FileInfo is the result of copying one file.
type FileInfo struct {
	Size   int64
	SHA256 string
}

// joinPackage resolves a manifest-relative path below packageDir.
func joinPackage(packageDir, rel string) (string, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(packageDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(base, filepath.FromSlash(clean))
	if !within(base, full) {
		return "", fmt.Errorf("path keluar paket: %s", rel)
	}
	return full, nil
}

// joinTarget resolves a target path below root.
func joinTarget(root, rel string) (string, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(base, filepath.FromSlash(clean))
	if !within(base, full) {
		return "", fmt.Errorf("path keluar generation: %s", rel)
	}
	return full, nil
}

func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
