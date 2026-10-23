//go:build !windows && !linux

package transfer

import "errors"

// ListDrives is not implemented on this platform.
func ListDrives() ([]Drive, error) {
	return nil, errors.New("transfer: deteksi drive belum didukung di platform ini")
}
