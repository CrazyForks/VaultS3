// Package sysinfo reports host-level facts VaultS3 exposes for operations, such
// as the disk capacity backing its data directories (a single-node equivalent of
// the capacity numbers `mc admin info` shows).
package sysinfo

// Disk is aggregated capacity across the distinct filesystems backing a set of
// paths, deduplicated by device so directories on the same disk are counted
// once. All values are bytes.
type Disk struct {
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

// DiskUsage aggregates total/free/used across the distinct filesystems backing
// the given paths. Empty or unreadable paths are skipped, so a partly-configured
// set of directories still returns what it can.
func DiskUsage(paths []string) Disk {
	var d Disk
	seen := map[uint64]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		total, free, dev, err := statfsPath(p)
		if err != nil {
			continue
		}
		if dev != 0 {
			if seen[dev] {
				continue // same filesystem already counted
			}
			seen[dev] = true
		}
		d.TotalBytes += total
		d.FreeBytes += free
	}
	if d.TotalBytes >= d.FreeBytes {
		d.UsedBytes = d.TotalBytes - d.FreeBytes
	}
	return d
}
