package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// runInfo prints server version and storage capacity (a single-node overview,
// like the capacity numbers `mc admin info` shows).
func runInfo(_ []string) {
	requireCreds()

	resp, err := apiRequest("GET", "/system", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var info struct {
		Version  string   `json:"version"`
		OS       string   `json:"os"`
		Arch     string   `json:"arch"`
		DataDirs []string `json:"dataDirs"`
		Disk     struct {
			TotalBytes uint64 `json:"totalBytes"`
			UsedBytes  uint64 `json:"usedBytes"`
			FreeBytes  uint64 `json:"freeBytes"`
		} `json:"disk"`
		ObjectBytes int64 `json:"objectBytes"`
		ObjectCount int64 `json:"objectCount"`
		BucketCount int   `json:"bucketCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		fatal(err.Error())
	}

	var pct float64
	if info.Disk.TotalBytes > 0 {
		pct = float64(info.Disk.UsedBytes) / float64(info.Disk.TotalBytes) * 100
	}

	fmt.Printf("VaultS3 %s (%s/%s)\n", info.Version, info.OS, info.Arch)
	fmt.Printf("Endpoint:   %s\n", endpoint)
	fmt.Printf("Buckets:    %d\n", info.BucketCount)
	fmt.Printf("Objects:    %d (%s logical)\n", info.ObjectCount, humanBytes(uint64(info.ObjectBytes)))
	fmt.Println("Disk capacity:")
	fmt.Printf("  Used:     %s (%.1f%%)\n", humanBytes(info.Disk.UsedBytes), pct)
	fmt.Printf("  Free:     %s\n", humanBytes(info.Disk.FreeBytes))
	fmt.Printf("  Total:    %s\n", humanBytes(info.Disk.TotalBytes))
	if len(info.DataDirs) > 0 {
		fmt.Printf("  Dirs:     %s\n", strings.Join(info.DataDirs, ", "))
	}
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
