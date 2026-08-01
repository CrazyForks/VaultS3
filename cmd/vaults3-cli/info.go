package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type diskInfo struct {
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

type dirUsage struct {
	Path  string `json:"path"`
	Bytes uint64 `json:"bytes"`
	Files uint64 `json:"files"`
	Error string `json:"error"`
}

type nodeUsage struct {
	Dirs      []dirUsage `json:"dirs"`
	Bytes     uint64     `json:"bytes"`
	Files     uint64     `json:"files"`
	ScannedAt time.Time  `json:"scannedAt"`
}

// runInfo prints server version and storage usage. It reports three different
// sizes on purpose, because reading one as another is what made a healthy
// cluster look like it had lost 2 TB (issue #43): the logical size of the
// objects, the space VaultS3's own directories occupy, and how full the
// underlying filesystems are.
func runInfo(_ []string) {
	requireCreds()

	resp, err := apiRequest("GET", "/cluster/info", nil)
	if err != nil {
		fatal(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fatal(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
	}

	var ci struct {
		Clustered      bool `json:"clustered"`
		NodeCount      int  `json:"nodeCount"`
		ReachableNodes int  `json:"reachableNodes"`
		Nodes          []struct {
			NodeID        string     `json:"nodeId"`
			Reachable     bool       `json:"reachable"`
			Error         string     `json:"error"`
			Version       string     `json:"version"`
			OS            string     `json:"os"`
			Arch          string     `json:"arch"`
			Disk          diskInfo   `json:"disk"`
			ObjectCount   int64      `json:"objectCount"`
			Usage         *nodeUsage `json:"usage"`
			UsageScanning bool       `json:"usageScanning"`
		} `json:"nodes"`
		Totals struct {
			Disk          diskInfo `json:"disk"`
			ObjectBytes   int64    `json:"objectBytes"`
			ObjectCount   int64    `json:"objectCount"`
			VaultBytes    uint64   `json:"vaultBytes"`
			VaultFiles    uint64   `json:"vaultFiles"`
			MeasuredNodes int      `json:"measuredNodes"`
		} `json:"totals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ci); err != nil {
		fatal(err.Error())
	}

	var ver, osName, arch string
	var scanning bool
	if len(ci.Nodes) > 0 {
		ver, osName, arch = ci.Nodes[0].Version, ci.Nodes[0].OS, ci.Nodes[0].Arch
		scanning = ci.Nodes[0].UsageScanning
	}
	fmt.Printf("VaultS3 %s (%s/%s)\n", ver, osName, arch)
	fmt.Printf("Endpoint:   %s\n", endpoint)
	if ci.Clustered {
		fmt.Printf("Cluster:    %d nodes (%d reachable)\n", ci.NodeCount, ci.ReachableNodes)
	}

	fmt.Println("\nStorage (three different measurements, they are not meant to match):")
	fmt.Printf("  %-18s %-12s %s\n", "Logical objects", humanBytes(uint64(ci.Totals.ObjectBytes)),
		fmt.Sprintf("%d objects, current versions, counted once", ci.Totals.ObjectCount))

	switch {
	case ci.Totals.MeasuredNodes == 0:
		fmt.Printf("  %-18s %-12s %s\n", "VaultS3 on disk", "unmeasured", scanHint(scanning))
	default:
		coverage := "on this node"
		if ci.Clustered {
			coverage = fmt.Sprintf("measured on %d/%d nodes", ci.Totals.MeasuredNodes, ci.ReachableNodes)
		}
		if ci.Totals.ObjectBytes > 0 && ci.Totals.MeasuredNodes == ci.ReachableNodes {
			coverage += fmt.Sprintf(", %.2fx logical", float64(ci.Totals.VaultBytes)/float64(ci.Totals.ObjectBytes))
		}
		fmt.Printf("  %-18s %-12s %s\n", "VaultS3 on disk", humanBytes(ci.Totals.VaultBytes), coverage)
	}

	var pct float64
	if ci.Totals.Disk.TotalBytes > 0 {
		pct = float64(ci.Totals.Disk.UsedBytes) / float64(ci.Totals.Disk.TotalBytes) * 100
	}
	fmt.Printf("  %-18s %-12s %s\n", "Filesystems",
		fmt.Sprintf("%s used", humanBytes(ci.Totals.Disk.UsedBytes)),
		fmt.Sprintf("%.1f%% of %s, includes anything else on those volumes",
			pct, humanBytes(ci.Totals.Disk.TotalBytes)))

	// The per-directory split is the fastest way to tell object data apart from
	// metadata and Raft logs when the footprint looks larger than expected.
	if len(ci.Nodes) > 0 && ci.Nodes[0].Usage != nil && len(ci.Nodes[0].Usage.Dirs) > 0 {
		// The age matters: the walk is cached, so a number read seconds after a
		// large upload legitimately lags behind it.
		fmt.Printf("\nThis node's directories (measured %s ago):\n", shortDuration(time.Since(ci.Nodes[0].Usage.ScannedAt)))
		for _, d := range ci.Nodes[0].Usage.Dirs {
			if d.Error != "" {
				fmt.Printf("  %-32s unreadable (%s)\n", d.Path, d.Error)
				continue
			}
			fmt.Printf("  %-32s %-12s %d files\n", d.Path, humanBytes(d.Bytes), d.Files)
		}
	}

	if ci.Clustered {
		fmt.Println("\nNodes:")
		for _, n := range ci.Nodes {
			if !n.Reachable {
				reason := n.Error
				if reason == "" {
					reason = "unreachable"
				}
				fmt.Printf("  %-24s unreachable (%s)\n", n.NodeID, reason)
				continue
			}
			vault := "measuring"
			if n.Usage != nil {
				vault = humanBytes(n.Usage.Bytes)
			}
			fmt.Printf("  %-24s %-10s vaults3 %-10s  fs %s / %s\n", n.NodeID, n.Version, vault,
				humanBytes(n.Disk.UsedBytes), humanBytes(n.Disk.TotalBytes))
		}
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func scanHint(scanning bool) string {
	if scanning {
		return "first scan in progress, run again in a moment"
	}
	return "disabled (storage.usage_scan_interval_secs = 0)"
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
