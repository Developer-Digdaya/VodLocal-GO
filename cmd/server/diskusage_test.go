package main

import "testing"

func TestDiskUsage(t *testing.T) {
	dir := t.TempDir()

	used, total, err := diskUsage(dir)
	if err != nil {
		t.Fatalf("diskUsage(%q) error: %v", dir, err)
	}
	if total <= 0 {
		t.Fatalf("kapasitas total = %d, ingin > 0", total)
	}
	if used < 0 || used > total {
		t.Fatalf("pemakaian = %d di luar rentang 0..%d", used, total)
	}
}
