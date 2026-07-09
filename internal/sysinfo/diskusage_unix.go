//go:build !windows

package sysinfo

import "syscall"

// statfsPath returns the total and (unprivileged-)available bytes of the
// filesystem containing path, plus its device id for deduplication.
func statfsPath(path string) (total, free, dev uint64, err error) {
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return 0, 0, 0, err
	}
	bsize := uint64(fs.Bsize)
	total = uint64(fs.Blocks) * bsize
	free = uint64(fs.Bavail) * bsize // available to non-root, matching `df`

	var st syscall.Stat_t
	if syscall.Stat(path, &st) == nil {
		dev = uint64(st.Dev)
	}
	return total, free, dev, nil
}
