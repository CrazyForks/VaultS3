// Package reclaim finds and removes object data on this node's disk that no
// metadata refers to any more.
//
// Metadata is authoritative (issue #34): a data file with no metadata entry is
// never served, never listed, and cannot be reached by any S3 call. Before issue
// #47 several delete paths removed the Raft-replicated metadata cluster-wide while
// deleting the data file only on the node that happened to serve the request, so
// on an N-node cluster (N-1)/N of every bulk-deleted byte turned into exactly that:
// unreachable files, growing without bound under a delete-heavy workload. Those
// paths now reap cluster-wide, but that only stops new leaks. This package is how
// an operator gets the already-stranded bytes back.
//
// Safety comes from three rules, in order of importance:
//
//  1. Only files with NO metadata at all are candidates. A file whose object still
//     exists is left alone even if this node is not its hash owner, because with
//     replica_count > 1 (and for reads that fall back to any holder, issue #42)
//     those extra copies are load-bearing.
//  2. Nothing newer than MinAge is touched. A PUT writes the data file before its
//     metadata commits, so a just-written object looks exactly like an orphan for
//     a moment. Hours of slack makes that race unreachable.
//  3. Dry run is the default everywhere above this package.
package reclaim

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// versionsDir is the per-bucket subdirectory holding non-current versions, laid
// out as <bucket>/.vs/<key>/<versionID>.
const versionsDir = ".vs"

// multipartDir holds in-progress upload parts, laid out as
// .multipart/<uploadID>/<part>. It sits beside the buckets, not inside one.
const multipartDir = ".multipart"

// tmpPrefix marks a half-written object still being streamed to disk. These are
// renamed into place on success and removed on failure, so they are never orphans
// and must never be deleted out from under an in-flight write.
const tmpPrefix = ".vaults3-tmp-"

// packedVolumesDir holds the append-only volume files that small-object packing
// writes objects into (<dataDir>/_volumes/vol-*.dat plus index.db). It sits at the
// same level as the buckets and does NOT start with a dot, so without naming it
// here a scan would read it as a bucket and delete live volumes holding millions
// of packed objects.
const packedVolumesDir = "_volumes"

// erasureDir holds erasure-coded shards at <bucket>/.ec/<key>/shard-N. An
// erasure-coded object has no plain data file at all, only shards, so shards must
// never be judged against the plain-object metadata lookup: every one of them
// would look like an orphan.
const erasureDir = ".ec"

// reservedTopLevel are directories under DataDir that are never buckets. Anything
// this package does not positively understand is skipped rather than deleted.
func reservedTopLevel(name string) bool {
	return name == multipartDir || name == packedVolumesDir ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// Lookup answers whether metadata still refers to a piece of data. Implemented by
// the metadata store; kept as an interface so this package does not depend on it
// and can be tested with a map.
type Lookup interface {
	// HasObject reports whether a current object exists at bucket/key.
	HasObject(bucket, key string) bool
	// HasVersion reports whether a specific version still exists.
	HasVersion(bucket, key, versionID string) bool
	// HasUpload reports whether an in-progress multipart upload still exists.
	// Multipart state is node-local (issue #32), so this is a local answer.
	HasUpload(uploadID string) bool
}

// Orphan is a single reclaimable path.
type Orphan struct {
	Path    string    `json:"path"`
	Bucket  string    `json:"bucket,omitempty"`
	Key     string    `json:"key,omitempty"`
	Version string    `json:"version,omitempty"`
	Upload  string    `json:"upload,omitempty"`
	Bytes   uint64    `json:"bytes"`
	ModTime time.Time `json:"modTime"`
}

// Report is the outcome of one scan.
type Report struct {
	// Scanned is every regular file considered, orphan or not.
	Scanned uint64 `json:"scanned"`
	// Orphans and OrphanBytes count what has no metadata and is old enough.
	Orphans     uint64 `json:"orphans"`
	OrphanBytes uint64 `json:"orphanBytes"`
	// Skipped counts files that looked orphaned but were too new to touch, which
	// is the expected state of a busy cluster and not a problem.
	SkippedTooNew uint64 `json:"skippedTooNew"`
	// Deleted and DeletedBytes are zero on a dry run.
	Deleted      uint64 `json:"deleted"`
	DeletedBytes uint64 `json:"deletedBytes"`
	// ByBucket breaks orphan bytes down per bucket ("" = multipart parts).
	ByBucket map[string]uint64 `json:"byBucket,omitempty"`
	// Samples are the largest orphans, so an operator can eyeball what would go
	// before running for real. Capped at maxSamples.
	Samples []Orphan `json:"samples,omitempty"`
	// Errors are per-path failures; a scan continues past them.
	Errors []string `json:"errors,omitempty"`
	// DryRun records how this scan ran, so a caller cannot misread the numbers.
	DryRun    bool      `json:"dryRun"`
	MinAge    string    `json:"minAge"`
	StartedAt time.Time `json:"startedAt"`
	TookMs    int64     `json:"tookMs"`
}

const maxSamples = 20

// Options configures a scan.
type Options struct {
	// DataDir is the root holding <bucket>/... and .multipart/.
	DataDir string
	// MinAge protects recently written files from the write-then-commit race.
	// A zero or negative value is refused rather than silently defaulted, so a
	// caller cannot delete live data by forgetting a field.
	MinAge time.Duration
	// DryRun reports without deleting.
	DryRun bool
	// Buckets, when non-empty, limits the scan to these buckets.
	Buckets []string
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// ErrNoMinAge is returned when Options.MinAge is not set.
var ErrNoMinAge = errors.New("reclaim: MinAge must be positive (refusing to delete recently written data)")

// Run scans DataDir and reports (and optionally removes) data with no metadata.
func Run(opts Options, look Lookup) (*Report, error) {
	if opts.MinAge <= 0 {
		return nil, ErrNoMinAge
	}
	if opts.DataDir == "" {
		return nil, errors.New("reclaim: DataDir is required")
	}
	if look == nil {
		return nil, errors.New("reclaim: a metadata Lookup is required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	start := nowFn()
	cutoff := start.Add(-opts.MinAge)

	rep := &Report{
		ByBucket:  map[string]uint64{},
		DryRun:    opts.DryRun,
		MinAge:    opts.MinAge.String(),
		StartedAt: start,
	}

	only := map[string]bool{}
	for _, b := range opts.Buckets {
		only[b] = true
	}

	entries, err := os.ReadDir(opts.DataDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == multipartDir {
			if len(only) == 0 {
				scanMultipart(filepath.Join(opts.DataDir, name), cutoff, look, rep)
			}
			continue
		}
		// Anything else that is not a bucket is internal bookkeeping this package
		// does not model; leaving it alone is always the safe choice.
		if reservedTopLevel(name) {
			continue
		}
		if len(only) > 0 && !only[name] {
			continue
		}
		scanBucket(filepath.Join(opts.DataDir, name), name, cutoff, look, rep)
	}

	finish(rep, nowFn().Sub(start))
	return rep, nil
}

// scanBucket walks one bucket directory. Plain objects live at <bucket>/<key>;
// versions live at <bucket>/.vs/<key>/<versionID>.
func scanBucket(root, bucket string, cutoff time.Time, look Lookup, rep *Report) {
	vsRoot := filepath.Join(root, versionsDir)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			rep.Errors = append(rep.Errors, path+": "+err.Error())
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rep.Scanned++

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Only two in-bucket layouts are understood: a plain object at <key>, and a
		// version under .vs/. Every other dot-segment belongs to a feature with its
		// own on-disk shape (.ec holds erasure shards, which have no plain-object
		// metadata entry at all and would look orphaned to the last byte), so it is
		// skipped rather than guessed at.
		if seg := firstDotSegment(rel); seg != "" && seg != versionsDir {
			return nil
		}

		var o Orphan
		if strings.HasPrefix(path, vsRoot+string(filepath.Separator)) {
			// <bucket>/.vs/<key>/<versionID>
			vrel, e := filepath.Rel(vsRoot, path)
			if e != nil {
				return nil
			}
			vrel = filepath.ToSlash(vrel)
			idx := strings.LastIndex(vrel, "/")
			if idx <= 0 {
				return nil
			}
			key, version := vrel[:idx], vrel[idx+1:]
			if look.HasVersion(bucket, key, version) {
				return nil
			}
			o = Orphan{Bucket: bucket, Key: key, Version: version}
		} else {
			if look.HasObject(bucket, rel) {
				return nil
			}
			o = Orphan{Bucket: bucket, Key: rel}
		}
		o.Path = path
		record(rep, bucket, o, d, cutoff)
		return nil
	})
}

// firstDotSegment returns the first path segment starting with a dot, or "" when
// there is none. Object keys may legitimately contain dots ("a.parquet") but a
// leading-dot SEGMENT is how every internal layout marks itself.
func firstDotSegment(rel string) string {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return seg
		}
	}
	return ""
}

// scanMultipart walks .multipart/<uploadID>/... Parts whose upload record is gone
// are unreachable: no ListParts, no abort, no lifecycle rule can find them. On a
// cluster this also catches uploads stranded by a ring change (issue #47 bug B).
func scanMultipart(root string, cutoff time.Time, look Lookup, rep *Report) {
	uploads, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			rep.Errors = append(rep.Errors, root+": "+err.Error())
		}
		return
	}
	for _, u := range uploads {
		if !u.IsDir() {
			continue
		}
		uploadID := u.Name()
		if look.HasUpload(uploadID) {
			continue
		}
		dir := filepath.Join(root, uploadID)
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				rep.Errors = append(rep.Errors, path+": "+err.Error())
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rep.Scanned++
			record(rep, "", Orphan{Path: path, Upload: uploadID}, d, cutoff)
			return nil
		})
	}
}

// record applies the age guard and accounts (and optionally deletes) one orphan.
func record(rep *Report, bucket string, o Orphan, d fs.DirEntry, cutoff time.Time) {
	info, err := d.Info()
	if err != nil {
		rep.Errors = append(rep.Errors, o.Path+": "+err.Error())
		return
	}
	// The write-then-commit window: a PUT lands the bytes before the metadata
	// commits, so a brand new object is indistinguishable from an orphan.
	if info.ModTime().After(cutoff) {
		rep.SkippedTooNew++
		return
	}
	o.Bytes = uint64(info.Size())
	o.ModTime = info.ModTime()

	rep.Orphans++
	rep.OrphanBytes += o.Bytes
	rep.ByBucket[bucket] += o.Bytes
	rep.Samples = append(rep.Samples, o)

	if !rep.DryRun {
		if err := os.Remove(o.Path); err != nil {
			rep.Errors = append(rep.Errors, o.Path+": "+err.Error())
			return
		}
		rep.Deleted++
		rep.DeletedBytes += o.Bytes
	}
}

// finish trims the sample list to the largest few and stamps the duration.
func finish(rep *Report, took time.Duration) {
	sort.Slice(rep.Samples, func(i, j int) bool { return rep.Samples[i].Bytes > rep.Samples[j].Bytes })
	if len(rep.Samples) > maxSamples {
		rep.Samples = rep.Samples[:maxSamples]
	}
	rep.TookMs = took.Milliseconds()
}
