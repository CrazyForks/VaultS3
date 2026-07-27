package erasure

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// shardStream serves an erasure-coded object by streaming its data shards in
// order, so a GET emits its first byte after reading only the first shard block
// rather than after reading and reassembling the whole object (issue #38).
//
// This is possible because the code is systematic Reed-Solomon: Split() writes the
// original bytes into the data shards unchanged (equal sized, the last one
// zero-padded) and Join() simply concatenates data shards 0..k-1 and truncates to
// OriginalSize. So the plaintext at logical offset o lives in data shard
// o/perShard at offset o%perShard, and no parity math is needed while every data
// shard is intact.
//
// If any data shard turns out to be missing or unreadable, the stream transparently
// falls back to a full read-and-reconstruct (parity recovery) and continues from the
// same logical offset, so a degraded read still returns correct bytes. Cross-shard
// parity verification is not run on the healthy read path (it would require reading
// every shard, which is exactly the cost being removed); the background Healer scans
// for and repairs degraded objects instead.
type shardStream struct {
	e      *Engine
	bucket string
	key    string
	meta   *ShardMeta

	perShard int64 // size of each data shard (all data shards are equal sized)
	size     int64 // OriginalSize: logical length of the object
	pos      int64 // current logical read offset

	cur    storage.ReadSeekCloser // currently open data shard, nil if none
	curIdx int                    // index of the open shard, -1 when none
	curPos int64                  // offset within the open shard

	// fallback holds the fully reconstructed object once a degraded shard forced
	// parity recovery; when set it is the source of truth for all further reads.
	fallback []byte
}

// newShardStream builds a streaming reader when every data shard is present.
// Returns false when the object cannot be streamed (unexpected shard layout or a
// missing data shard), in which case the caller uses the reconstructing path.
func (e *Engine) newShardStream(bucket, key string, meta *ShardMeta) (*shardStream, bool) {
	if meta.DataShards <= 0 || len(meta.ShardSizes) < meta.DataShards {
		return nil, false
	}
	perShard := meta.ShardSizes[0]
	if perShard <= 0 {
		return nil, false
	}
	// The offset math assumes uniformly sized data shards, which is what Split
	// produces. Anything else falls back rather than risking a wrong mapping.
	for i := 0; i < meta.DataShards; i++ {
		if meta.ShardSizes[i] != perShard {
			return nil, false
		}
	}
	if perShard*int64(meta.DataShards) < meta.OriginalSize {
		return nil, false
	}
	// Cheap presence check (a stat per data shard, no data read). A shard that
	// exists but fails to open later is handled by the in-stream fallback.
	for i := 0; i < meta.DataShards; i++ {
		if !e.backendFor(i).ObjectExists(bucket, shardKey(key, i)) {
			return nil, false
		}
	}
	return &shardStream{
		e: e, bucket: bucket, key: key, meta: meta,
		perShard: perShard, size: meta.OriginalSize, curIdx: -1,
	}, true
}

func (s *shardStream) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if s.fallback != nil {
		n := copy(p, s.fallback[s.pos:s.size])
		s.pos += int64(n)
		return n, nil
	}

	idx := int(s.pos / s.perShard)
	off := s.pos % s.perShard

	if s.curIdx != idx {
		s.closeCur()
		rc, _, err := s.e.backendFor(idx).GetObject(s.bucket, shardKey(s.key, idx))
		if err != nil {
			return s.recoverAndRead(p, fmt.Errorf("open data shard %d: %w", idx, err))
		}
		s.cur, s.curIdx, s.curPos = rc, idx, 0
	}
	if s.curPos != off {
		if _, err := s.cur.Seek(off, io.SeekStart); err != nil {
			return s.recoverAndRead(p, fmt.Errorf("seek data shard %d: %w", idx, err))
		}
		s.curPos = off
	}

	// Never read past this shard's end (the next bytes live in the next shard) or
	// past the object's logical end (the last data shard is zero-padded).
	limit := s.perShard - off
	if rem := s.size - s.pos; rem < limit {
		limit = rem
	}
	if int64(len(p)) > limit {
		p = p[:limit]
	}

	n, err := s.cur.Read(p)
	s.pos += int64(n)
	s.curPos += int64(n)
	if err == io.EOF {
		// The shard ended: fine if we consumed it exactly (the next Read moves to
		// the next shard), but a genuinely short shard means it is damaged.
		if n > 0 || s.curPos >= s.perShard {
			return n, nil
		}
		return s.recoverAndRead(p, fmt.Errorf("data shard %d is short", idx))
	}
	if err != nil {
		if n > 0 {
			return n, nil
		}
		return s.recoverAndRead(p, fmt.Errorf("read data shard %d: %w", idx, err))
	}
	return n, nil
}

// recoverAndRead reconstructs the whole object from parity after a data shard turned
// out to be unusable, then serves the current read from it. This keeps a degraded
// read correct (identical bytes to the old always-reconstruct path) at the cost of
// buffering, which only happens when the fast path actually fails.
func (s *shardStream) recoverAndRead(p []byte, cause error) (int, error) {
	s.closeCur()
	slog.Warn("erasure: falling back to parity reconstruction for read",
		"bucket", s.bucket, "key", s.key, "reason", cause)

	data, err := s.e.reconstruct(s.bucket, s.key, s.meta)
	if err != nil {
		return 0, fmt.Errorf("erasure: %w (after %v)", err, cause)
	}
	if int64(len(data)) < s.size {
		return 0, fmt.Errorf("erasure: reconstructed %d bytes, expected %d", len(data), s.size)
	}
	s.fallback = data
	if s.pos >= s.size {
		return 0, io.EOF
	}
	n := copy(p, s.fallback[s.pos:s.size])
	s.pos += int64(n)
	return n, nil
}

func (s *shardStream) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, fmt.Errorf("erasure: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("erasure: negative seek position %d", abs)
	}
	// Reposition lazily: the next Read opens/seeks the right shard. Range and
	// partNumber reads therefore cost one seek, not a full materialization.
	s.pos = abs
	return abs, nil
}

func (s *shardStream) Close() error {
	s.closeCur()
	s.fallback = nil
	return nil
}

func (s *shardStream) closeCur() {
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
	s.curIdx = -1
	s.curPos = 0
}
