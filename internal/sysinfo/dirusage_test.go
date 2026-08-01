package sysinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanDirsCountsFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.bin"), 4096)
	write(t, filepath.Join(dir, "nested", "deep", "b.bin"), 8192)

	u := ScanDirs([]string{dir})
	if u.Files != 2 {
		t.Fatalf("files = %d, want 2", u.Files)
	}
	// Allocated blocks are at least the apparent size and, with directory
	// entries excluded, should not run away either.
	if u.Bytes < 12288 {
		t.Fatalf("bytes = %d, want >= 12288", u.Bytes)
	}
	if len(u.Dirs) != 1 || u.Dirs[0].Bytes != u.Bytes {
		t.Fatalf("per-dir breakdown does not match the total: %+v", u.Dirs)
	}
	if u.ScannedAt.IsZero() {
		t.Fatal("ScannedAt not stamped")
	}
}

// A metadata directory configured underneath the data directory must not be
// billed twice: the walk of the parent already covers it.
func TestScanDirsCountsNestedPathOnce(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "metadata")
	write(t, filepath.Join(dir, "a.bin"), 4096)
	write(t, filepath.Join(sub, "b.bin"), 4096)

	nested := ScanDirs([]string{dir, sub})
	parentOnly := ScanDirs([]string{dir})
	if nested.Bytes != parentOnly.Bytes || nested.Files != parentOnly.Files {
		t.Fatalf("nested path double counted: %d/%d vs %d/%d",
			nested.Bytes, nested.Files, parentOnly.Bytes, parentOnly.Files)
	}
	if len(nested.Dirs) != 1 {
		t.Fatalf("want the nested dir dropped from the breakdown, got %+v", nested.Dirs)
	}
}

func TestScanDirsSeparateDirsAreSummed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "a.bin"), 4096)
	write(t, filepath.Join(b, "b.bin"), 4096)

	u := ScanDirs([]string{a, b})
	if len(u.Dirs) != 2 || u.Files != 2 {
		t.Fatalf("want 2 dirs / 2 files, got %d dirs / %d files", len(u.Dirs), u.Files)
	}
	if u.Bytes != u.Dirs[0].Bytes+u.Dirs[1].Bytes {
		t.Fatalf("total %d != %d + %d", u.Bytes, u.Dirs[0].Bytes, u.Dirs[1].Bytes)
	}
}

// A hardlinked file occupies its blocks once, so counting it per link would
// invent storage that is not there.
func TestScanDirsCountsHardlinkOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink identity is not resolved on Windows")
	}
	dir := t.TempDir()
	orig := filepath.Join(dir, "a.bin")
	write(t, orig, 8192)
	before := ScanDirs([]string{dir}).Bytes

	if err := os.Link(orig, filepath.Join(dir, "a-link.bin")); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	after := ScanDirs([]string{dir})
	if after.Bytes != before {
		t.Fatalf("hardlink added %d bytes, want 0", after.Bytes-before)
	}
	if after.Files != 1 {
		t.Fatalf("files = %d, want 1", after.Files)
	}
}

// A symlink costs its own entry, not the size of what it points at, and must
// never be followed out of the data directory.
func TestScanDirsDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	outside, dir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "big.bin"), 65536)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	u := ScanDirs([]string{dir})
	if u.Files != 0 || u.Bytes != 0 {
		t.Fatalf("symlink target counted: %d files / %d bytes", u.Files, u.Bytes)
	}
}

func TestScanDirsReportsMissingDirWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.bin"), 4096)

	u := ScanDirs([]string{dir, filepath.Join(dir, "..", "does-not-exist")})
	if u.Files != 1 {
		t.Fatalf("a missing dir voided the good one: %d files", u.Files)
	}
	var missing *DirUsage
	for i := range u.Dirs {
		if u.Dirs[i].Error != "" {
			missing = &u.Dirs[i]
		}
	}
	if missing == nil {
		t.Fatal("missing directory reported no error")
	}
}

func TestScanDirsIgnoresEmptyPaths(t *testing.T) {
	u := ScanDirs([]string{"", ""})
	if len(u.Dirs) != 0 || u.Bytes != 0 {
		t.Fatalf("empty paths produced %+v", u)
	}
}

func TestNewUsageCacheDisabledByZeroInterval(t *testing.T) {
	if c := NewUsageCache(func() []string { return nil }, 0); c != nil {
		t.Fatal("interval 0 should disable the cache")
	}
	// A nil cache must still be callable: that is how "disabled" is expressed.
	var c *UsageCache
	if u, scanning := c.Get(); u != nil || scanning {
		t.Fatalf("nil cache returned %+v / %v", u, scanning)
	}
	if u := c.Refresh(); u != nil {
		t.Fatalf("nil cache refreshed to %+v", u)
	}
}

func TestUsageCacheServesCachedScan(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.bin"), 4096)
	c := NewUsageCache(func() []string { return []string{dir} }, time.Hour)

	// The first read never blocks, so it reports "measuring" with nothing yet.
	if u, scanning := c.Get(); u != nil || !scanning {
		t.Fatalf("first Get returned %+v / scanning=%v", u, scanning)
	}
	waitForScan(t, c)

	u, scanning := c.Get()
	if u == nil || u.Files != 1 {
		t.Fatalf("cached scan = %+v", u)
	}
	if scanning {
		t.Fatal("a fresh scan should not trigger another within minAge")
	}

	// A file added after the scan must not appear until the value ages out.
	write(t, filepath.Join(dir, "b.bin"), 4096)
	if u, _ := c.Get(); u.Files != 1 {
		t.Fatalf("cache rescanned inside minAge: %d files", u.Files)
	}
}

func TestUsageCacheRescansOnceStale(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.bin"), 4096)
	c := NewUsageCache(func() []string { return []string{dir} }, time.Nanosecond)

	c.Refresh()
	write(t, filepath.Join(dir, "b.bin"), 4096)
	c.Get() // stale, so this kicks a background rescan

	if u := waitForScan(t, c); u.Files != 2 {
		t.Fatalf("stale cache did not pick up the new file: %d files", u.Files)
	}
}

// waitForScan reads the cached state directly rather than through Get, which
// would itself trigger a rescan and never settle at a short minAge.
func waitForScan(t *testing.T, c *UsageCache) *Usage {
	t.Helper()
	for i := 0; i < 200; i++ {
		c.mu.Lock()
		u, scanning := c.last, c.scanning
		c.mu.Unlock()
		if u != nil && !scanning {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background scan did not finish")
	return nil
}

func TestTopLevelPathsDropsNestedAndDuplicates(t *testing.T) {
	base := t.TempDir()
	got := topLevelPaths([]string{
		filepath.Join(base, "data"),
		filepath.Join(base, "data"),         // duplicate
		filepath.Join(base, "data", "meta"), // nested
		filepath.Join(base, "data-cold"),    // shares a prefix but is NOT nested
	})
	want := []string{filepath.Join(base, "data"), filepath.Join(base, "data-cold")}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
