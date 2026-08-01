//go:build windows

package sysinfo

import "io/fs"

// diskBytes falls back to the apparent size on Windows, where allocated-size
// and hardlink identity need a per-file CreateFile/GetFileInformationByHandle
// round trip that would make walking a large data directory far slower than the
// accuracy is worth.
func diskBytes(info fs.FileInfo) (uint64, fileKey) {
	return uint64(info.Size()), fileKey{}
}
