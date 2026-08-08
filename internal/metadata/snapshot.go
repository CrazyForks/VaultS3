package metadata

import (
	"encoding/binary"
	"fmt"
	"io"

	bolt "go.etcd.io/bbolt"
)

// WriteSnapshot writes the entire BoltDB database to w for Raft snapshots.
// Format: sequence of (bucketNameLen uint32, bucketName, numKV uint64, [(keyLen uint32, key, valLen uint32, val)]...)
func (s *Store) WriteSnapshot(w io.Writer) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			// Write bucket name
			if err := writeBytes(w, name); err != nil {
				return fmt.Errorf("write bucket name %s: %w", name, err)
			}

			// Count keys
			var count uint64
			b.ForEach(func(k, v []byte) error {
				count++
				return nil
			})

			// Write key count
			if err := binary.Write(w, binary.BigEndian, count); err != nil {
				return fmt.Errorf("write key count: %w", err)
			}

			// Write each key-value pair
			return b.ForEach(func(k, v []byte) error {
				if err := writeBytes(w, k); err != nil {
					return err
				}
				return writeBytes(w, v)
			})
		})
	})
}

// restoreStateBucket records that a snapshot restore is under way. It is written
// before the old state is dropped and removed only once the restore finishes, so
// a DB left behind by an interrupted restore is recognisable on the next open
// instead of quietly serving half a cluster's metadata.
var restoreStateBucket = []byte("_restore_state")

var restoreInProgressKey = []byte("in_progress")

// Restoring commits in bounded batches rather than one transaction. BoltDB holds
// every dirty page of a write transaction in memory until it commits, so a
// single-transaction restore cost roughly 1.4 KB of heap per object and scaled
// with the whole cluster's metadata: a node rejoining with a few hundred thousand
// objects allocated more than a gigabyte before committing anything, and was
// OOM-killed during startup before it ever served a request (issue #46 follow-up).
// Batching makes that cost flat.
// Vars rather than consts so tests can shrink them and exercise the batching
// without seeding a realistic amount of data.
var (
	restoreBatchBytes = 32 << 20
	restoreBatchKeys  = 20000
)

// RestoreSnapshot replaces the entire BoltDB state from a snapshot reader.
//
// The restore is NOT atomic: it commits as it goes, so an interrupted restore
// leaves a partial DB. That is deliberate, and safe, because the state is
// entirely derived from Raft — the sentinel below marks the DB as incomplete and
// the next open clears it, after which Raft installs its snapshot again. The
// alternative (one transaction) is atomic but allocates without bound, which is
// the failure this replaced.
func (s *Store) RestoreSnapshot(r io.Reader) error {
	if err := s.beginRestore(); err != nil {
		return err
	}

	for {
		name, err := readBytes(r)
		if err == io.EOF {
			return s.finishRestore()
		}
		if err != nil {
			return fmt.Errorf("read bucket name: %w", err)
		}

		var count uint64
		if err := binary.Read(r, binary.BigEndian, &count); err != nil {
			return fmt.Errorf("read key count: %w", err)
		}

		if err := s.restoreBucket(r, name, count); err != nil {
			return err
		}
	}
}

// beginRestore drops the existing state and marks the DB as mid-restore.
func (s *Store) beginRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var existing [][]byte
		tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			existing = append(existing, append([]byte{}, name...))
			return nil
		})
		for _, name := range existing {
			if err := tx.DeleteBucket(name); err != nil {
				return fmt.Errorf("delete bucket %s: %w", name, err)
			}
		}
		b, err := tx.CreateBucket(restoreStateBucket)
		if err != nil {
			return fmt.Errorf("create restore state: %w", err)
		}
		return b.Put(restoreInProgressKey, []byte("1"))
	})
}

// finishRestore clears the mid-restore marker, making the DB usable.
func (s *Store) finishRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(restoreStateBucket); b != nil {
			return b.Delete(restoreInProgressKey)
		}
		return nil
	})
}

// restoreBucket reads count key/value pairs for one bucket, committing every
// restoreBatchKeys keys or restoreBatchBytes of data so peak memory stays bounded
// by the batch rather than by the size of the snapshot.
func (s *Store) restoreBucket(r io.Reader, name []byte, count uint64) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(name)
		return err
	}); err != nil {
		return fmt.Errorf("create bucket %s: %w", name, err)
	}

	type pair struct{ key, val []byte }
	batch := make([]pair, 0, 1024)
	batchBytes := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(name)
			if b == nil {
				return fmt.Errorf("bucket %s vanished mid-restore", name)
			}
			for _, p := range batch {
				if err := b.Put(p.key, p.val); err != nil {
					return fmt.Errorf("put key: %w", err)
				}
			}
			return nil
		})
		batch = batch[:0]
		batchBytes = 0
		return err
	}

	for i := uint64(0); i < count; i++ {
		key, err := readBytes(r)
		if err != nil {
			return fmt.Errorf("read key: %w", err)
		}
		val, err := readBytes(r)
		if err != nil {
			return fmt.Errorf("read value: %w", err)
		}
		batch = append(batch, pair{key, val})
		batchBytes += len(key) + len(val)

		if len(batch) >= restoreBatchKeys || batchBytes >= restoreBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// restoreWasInterrupted reports whether a previous restore did not finish, which
// means the metadata in this DB is a partial copy of the cluster's and must not
// be served.
func (s *Store) restoreWasInterrupted() bool {
	interrupted := false
	s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(restoreStateBucket); b != nil {
			interrupted = b.Get(restoreInProgressKey) != nil
		}
		return nil
	})
	return interrupted
}

// clearInterruptedRestore empties the DB left by an interrupted restore. Raft
// installs its snapshot again on the next restore, so starting from empty is the
// correct recovery; keeping the partial data would mean serving a subset of the
// cluster's objects as if it were the whole set.
func (s *Store) clearInterruptedRestore() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var existing [][]byte
		tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			existing = append(existing, append([]byte{}, name...))
			return nil
		})
		for _, name := range existing {
			if err := tx.DeleteBucket(name); err != nil {
				return fmt.Errorf("delete bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

func writeBytes(w io.Writer, data []byte) error {
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readBytes(r io.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}
