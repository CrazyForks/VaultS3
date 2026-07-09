package api

import (
	"net/http"
	"runtime"

	"github.com/Kodiqa-Solutions/VaultS3/internal/sysinfo"
)

// handleSystemInfo handles GET /api/v1/system: version, data directories, on-disk
// capacity (total/used/free) and logical object usage. This is the single-node
// "how much capacity and how much is occupied" overview.
func (h *APIHandler) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	// Every configured data directory contributes to on-disk capacity; DiskUsage
	// deduplicates directories that share a filesystem.
	var dirs []string
	if h.cfg != nil {
		dirs = append(dirs, h.cfg.Storage.DataDir, h.cfg.Storage.MetadataDir)
		if h.cfg.Tiering.Enabled && h.cfg.Tiering.ColdDataDir != "" {
			dirs = append(dirs, h.cfg.Tiering.ColdDataDir)
		}
		if h.cfg.Erasure.Enabled {
			dirs = append(dirs, h.cfg.Erasure.DataDirs...)
		}
	}

	// Logical object usage (sum of object bytes, which differs from on-disk bytes
	// when compression or encryption is enabled).
	var objectBytes, objectCount int64
	var bucketCount int
	if buckets, err := h.store.ListBuckets(); err == nil {
		bucketCount = len(buckets)
		for _, b := range buckets {
			size, count := h.bucketStatCounter(b.Name)
			objectBytes += size
			objectCount += count
		}
	}

	version := "dev"
	if h.updater != nil {
		if v := h.updater.LastStatus().Current; v != "" {
			version = v
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":     version,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"dataDirs":    uniqueNonEmpty(dirs),
		"disk":        sysinfo.DiskUsage(dirs),
		"objectBytes": objectBytes,
		"objectCount": objectCount,
		"bucketCount": bucketCount,
	})
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
