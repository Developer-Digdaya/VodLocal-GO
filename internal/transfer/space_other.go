//go:build !windows && !linux

package transfer

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("transfer: free space tidak didukung di platform ini")
}
