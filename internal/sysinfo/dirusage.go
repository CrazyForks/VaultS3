package sysinfo

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DirUsage is the measured on-disk footprint of one directory VaultS3 owns.
type DirUsage struct {
	Path  string `json:"path"`
	Bytes uint64 `json:"bytes"`
	Files uint64 `json:"files"`
	Error string `json:"error,omitempty"`
}

// Usage is a completed walk of every directory VaultS3 writes to. It answers a
// question DiskUsage cannot: how much of a volume's used space is VaultS3's.
// DiskUsage is a statfs of the whole filesystem, so it also counts the OS,
// container images, logs, and anything else sharing the disk (issue #43).
type Usage struct {
	Dirs      []DirUsage `json:"dirs"`
	Bytes     uint64     `json:"bytes"`
	Files     uint64     `json:"files"`
	ScannedAt time.Time  `json:"scannedAt"`
	TookMs    int64      `json:"tookMs"`
}

// fileKey identifies a file by device and inode so a hardlinked file, which
// occupies its blocks once, is not counted once per link.
type fileKey struct {
	dev, ino uint64
	valid    bool
}

// ScanDirs walks paths and reports how many bytes they actually occupy. Sizes
// are allocated blocks rather than apparent length (what `du` reports), so a
// small object still costs a whole filesystem block and a sparse file costs
// only what it has been given. Nested paths are counted once.
func ScanDirs(paths []string) Usage {
	start := time.Now()
	u := Usage{Dirs: []DirUsage{}}
	seen := map[fileKey]bool{}
	for _, p := range topLevelPaths(paths) {
		d := scanDir(p, seen)
		u.Dirs = append(u.Dirs, d)
		u.Bytes += d.Bytes
		u.Files += d.Files
	}
	u.ScannedAt = time.Now()
	u.TookMs = time.Since(start).Milliseconds()
	return u
}

func scanDir(path string, seen map[fileKey]bool) DirUsage {
	d := DirUsage{Path: path}
	err := filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable subdirectory should not void the whole measurement;
			// only a failure on the root is worth reporting.
			if p == path {
				return err
			}
			return nil
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil // symlinks are not followed, so they cost only their entry
		}
		info, err := e.Info()
		if err != nil {
			return nil // vanished mid-walk (a delete or a compaction)
		}
		n, key := diskBytes(info)
		if key.valid {
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		d.Bytes += n
		d.Files++
		return nil
	})
	if err != nil {
		d.Error = err.Error()
	}
	return d
}

// topLevelPaths cleans the input and drops any path nested inside another, so a
// metadata directory configured underneath the data directory is not counted
// twice. Order is shortest-first, which is also the order operators read.
func topLevelPaths(paths []string) []string {
	cleaned := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = filepath.Clean(p)
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		cleaned = append(cleaned, abs)
	}
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) < len(cleaned[j]) })

	out := make([]string, 0, len(cleaned))
	for _, p := range cleaned {
		nested := false
		for _, kept := range out {
			if strings.HasPrefix(p, strings.TrimSuffix(kept, string(os.PathSeparator))+string(os.PathSeparator)) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, p)
		}
	}
	return out
}

// UsageCache serves the most recent completed scan and refreshes it in the
// background. Walking a data directory holding hundreds of thousands of objects
// takes seconds, so it must never run on a request goroutine: Get returns
// immediately with whatever it has and starts a rescan only once the cached
// value has aged out. A nil cache is usable and always reports "not measured",
// which is how the scan is disabled.
type UsageCache struct {
	paths  func() []string
	minAge time.Duration

	mu       sync.Mutex
	last     *Usage
	scanning bool
}

// NewUsageCache returns a cache that rescans at most once per minAge, on
// demand. A minAge of zero or less disables scanning entirely (nil cache).
func NewUsageCache(paths func() []string, minAge time.Duration) *UsageCache {
	if paths == nil || minAge <= 0 {
		return nil
	}
	return &UsageCache{paths: paths, minAge: minAge}
}

// Get returns the last completed scan (nil if none has finished yet) and
// whether one is running now. It never blocks.
func (c *UsageCache) Get() (*Usage, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.scanning && (c.last == nil || time.Since(c.last.ScannedAt) >= c.minAge) {
		c.scanning = true
		go c.Refresh()
	}
	return c.last, c.scanning
}

// Refresh walks the directories now and stores the result. Get calls it in the
// background; call it directly only when a fresh number is worth waiting for.
func (c *UsageCache) Refresh() *Usage {
	if c == nil {
		return nil
	}
	u := ScanDirs(c.paths())
	c.mu.Lock()
	c.last, c.scanning = &u, false
	c.mu.Unlock()
	return &u
}
