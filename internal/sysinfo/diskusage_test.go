package sysinfo

import "testing"

func TestDiskUsage(t *testing.T) {
	dir := t.TempDir()
	d := DiskUsage([]string{dir})
	if d.TotalBytes == 0 {
		t.Fatal("expected non-zero total disk capacity")
	}
	if d.FreeBytes > d.TotalBytes {
		t.Fatalf("free (%d) should not exceed total (%d)", d.FreeBytes, d.TotalBytes)
	}
	if d.UsedBytes != d.TotalBytes-d.FreeBytes {
		t.Fatalf("used (%d) should equal total-free (%d)", d.UsedBytes, d.TotalBytes-d.FreeBytes)
	}

	// The same filesystem must be counted once (dedup by device).
	single := DiskUsage([]string{dir})
	double := DiskUsage([]string{dir, dir})
	if single.TotalBytes != double.TotalBytes {
		t.Fatalf("dedup failed: single=%d double=%d", single.TotalBytes, double.TotalBytes)
	}

	// Empty and unreadable paths are skipped, not fatal.
	if got := DiskUsage([]string{"", "/no/such/path/xyzzy"}).TotalBytes; got != 0 {
		t.Fatalf("expected 0 for empty/invalid paths, got %d", got)
	}
}
