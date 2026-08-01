//go:build !windows

package sysinfo

import (
	"io/fs"
	"syscall"
)

// diskBytes reports what a file costs the filesystem, plus a key identifying it
// if it is hardlinked. st.Blocks is in 512-byte units regardless of the
// filesystem's block size (stat(2)), and counts allocated blocks: a 1-byte
// object still occupies a whole block, and a sparse file occupies less than its
// length. This is the number `du` reports and the one that has to add up to the
// volume's used space.
func diskBytes(info fs.FileInfo) (uint64, fileKey) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return uint64(info.Size()), fileKey{}
	}
	var key fileKey
	if st.Nlink > 1 {
		key = fileKey{dev: uint64(st.Dev), ino: uint64(st.Ino), valid: true}
	}
	if st.Blocks < 0 {
		return uint64(info.Size()), key
	}
	return uint64(st.Blocks) * 512, key
}
