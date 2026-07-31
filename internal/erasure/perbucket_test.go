package erasure

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// newPerBucketEngine builds an EC engine whose per-bucket policy is driven by the
// returned map, so a test can flip a bucket between coded and plain.
func newPerBucketEngine(t *testing.T, coded map[string]bool) (*Engine, storage.Engine) {
	t.Helper()
	inner, err := storage.NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	e, err := NewEngine(inner, config.ErasureConfig{
		Enabled: true, DataShards: 4, ParityShards: 2, BlockSize: 1024,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e.SetBucketPolicy(func(bucket string) bool { return coded[bucket] })
	for _, b := range []string{"coded", "plain"} {
		if err := e.CreateBucketDir(b); err != nil {
			t.Fatalf("CreateBucketDir: %v", err)
		}
	}
	return e, inner
}

func put(t *testing.T, e *Engine, bucket, key string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	if _, _, err := e.PutObject(bucket, key, bytes.NewReader(data), int64(size)); err != nil {
		t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
	return data
}

func readBack(t *testing.T, e *Engine, bucket, key string) []byte {
	t.Helper()
	r, _, err := e.GetObject(bucket, key)
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return got
}

// TestPerBucketErasureOptOut is the point of issue #39: a bucket holding data that
// is cheap to recreate should not pay for parity, while another bucket keeps it.
func TestPerBucketErasureOptOut(t *testing.T) {
	e, inner := newPerBucketEngine(t, map[string]bool{"coded": true, "plain": false})

	const size = 8192 // well above BlockSize, so it would normally be coded
	want := put(t, e, "coded", "obj", size)
	wantPlain := put(t, e, "plain", "obj", size)

	// The coded bucket really is coded: shard metadata exists.
	if !inner.ObjectExists("coded", metaKey("obj")) {
		t.Error("the erasure-enabled bucket should have written shard metadata")
	}
	// The opted-out bucket stored one plain object, no shards, no metadata.
	if inner.ObjectExists("plain", metaKey("obj")) {
		t.Error("the opted-out bucket must not write shard metadata")
	}
	if !inner.ObjectExists("plain", "obj") {
		t.Error("the opted-out bucket should hold the object as a single plain file")
	}

	// Both read back byte-identical, which is what makes the choice safe.
	if got := readBack(t, e, "coded", "obj"); !bytes.Equal(got, want) {
		t.Error("erasure-coded object did not round-trip")
	}
	if got := readBack(t, e, "plain", "obj"); !bytes.Equal(got, wantPlain) {
		t.Error("plain object did not round-trip")
	}
}

// TestPerBucketErasureSavesSpace measures the actual reason for the feature: with
// 4+2 coding an object occupies about 1.5x its size, and opting out removes that.
func TestPerBucketErasureSavesSpace(t *testing.T) {
	e, inner := newPerBucketEngine(t, map[string]bool{"coded": true, "plain": false})

	const size = 64 * 1024
	put(t, e, "coded", "obj", size)
	put(t, e, "plain", "obj", size)

	codedBytes, _, err := inner.BucketSize("coded")
	if err != nil {
		t.Fatalf("BucketSize coded: %v", err)
	}
	plainBytes, _, err := inner.BucketSize("plain")
	if err != nil {
		t.Fatalf("BucketSize plain: %v", err)
	}

	t.Logf("on disk for a %d byte object: erasure-coded %d bytes, opted out %d bytes", size, codedBytes, plainBytes)
	if plainBytes >= codedBytes {
		t.Fatalf("opting out should use less disk: coded=%d plain=%d", codedBytes, plainBytes)
	}
	// 4+2 means 6 shards for 4 data shards' worth of payload, so ~1.5x plus meta.
	if ratio := float64(codedBytes) / float64(plainBytes); ratio < 1.4 || ratio > 1.7 {
		t.Errorf("expected roughly 1.5x overhead from 4+2 coding, got %.2fx", ratio)
	}
}

// TestPerBucketErasureFlipKeepsOldObjectsReadable covers the migration story: the
// setting only steers new writes, and reads detect an object's layout from the
// object itself, so a bucket may hold both kinds at once.
func TestPerBucketErasureFlipKeepsOldObjectsReadable(t *testing.T) {
	policy := map[string]bool{"coded": true}
	e, _ := newPerBucketEngine(t, policy)

	before := put(t, e, "coded", "written-while-coded", 8192)

	// Operator turns erasure coding off for this bucket.
	policy["coded"] = false
	after := put(t, e, "coded", "written-while-plain", 8192)

	if got := readBack(t, e, "coded", "written-while-coded"); !bytes.Equal(got, before) {
		t.Error("an object written before the change must still read back correctly")
	}
	if got := readBack(t, e, "coded", "written-while-plain"); !bytes.Equal(got, after) {
		t.Error("an object written after the change must read back correctly")
	}

	// And back on again, with both older objects still readable.
	policy["coded"] = true
	third := put(t, e, "coded", "written-while-coded-again", 8192)
	for key, want := range map[string][]byte{
		"written-while-coded":       before,
		"written-while-plain":       after,
		"written-while-coded-again": third,
	} {
		if got := readBack(t, e, "coded", key); !bytes.Equal(got, want) {
			t.Errorf("%s did not round-trip after the setting changed twice", key)
		}
	}
}

// TestPerBucketErasureDeleteRemovesShards makes sure the opt-out does not confuse
// deletion, which has to clean up whichever layout the object actually used.
func TestPerBucketErasureDeleteRemovesShards(t *testing.T) {
	policy := map[string]bool{"coded": true}
	e, inner := newPerBucketEngine(t, policy)

	put(t, e, "coded", "a", 8192)
	policy["coded"] = false
	put(t, e, "coded", "b", 8192)

	for _, key := range []string{"a", "b"} {
		if err := e.DeleteObject("coded", key); err != nil {
			t.Fatalf("DeleteObject %s: %v", key, err)
		}
	}

	size, count, err := inner.BucketSize("coded")
	if err != nil {
		t.Fatalf("BucketSize: %v", err)
	}
	if size != 0 || count != 0 {
		t.Fatalf("deleting both objects should leave the bucket empty, got %d bytes in %d files", size, count)
	}
}

// TestErasureDefaultsToCodedWithoutPolicy keeps the previous behaviour for a
// server that never sets a policy.
func TestErasureDefaultsToCodedWithoutPolicy(t *testing.T) {
	inner, err := storage.NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	e, err := NewEngine(inner, config.ErasureConfig{
		Enabled: true, DataShards: 4, ParityShards: 2, BlockSize: 1024,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := e.CreateBucketDir("b"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}
	put(t, e, "b", "obj", 8192)
	if !inner.ObjectExists("b", metaKey("obj")) {
		t.Error("with no per-bucket policy every bucket must stay erasure coded")
	}
}
