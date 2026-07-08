package storage

import (
	"io"
	"strings"
	"testing"
)

// TestDirMarkerAcrossEngines is the regression for the FreeBSD folder bug: an S3
// directory marker (key ending in "/") stored as a file blocked child objects
// with "mkdir ...: not a directory" (ENOTDIR). Markers must map to directories
// so children nest, read back as zero bytes, and be deletable. Verified through
// the plain engine and the transforming wrappers (compressed, encrypted).
func TestDirMarkerAcrossEngines(t *testing.T) {
	engines := map[string]func(t *testing.T) Engine{
		"filesystem": func(t *testing.T) Engine {
			fs, err := NewFileSystem(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return fs
		},
		"compressed": func(t *testing.T) Engine {
			fs, err := NewFileSystem(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return NewCompressedEngine(fs)
		},
		"encrypted": func(t *testing.T) Engine {
			fs, err := NewFileSystem(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			enc, err := NewEncryptedEngine(fs, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			return enc
		},
	}

	for name, mk := range engines {
		t.Run(name, func(t *testing.T) {
			e := mk(t)
			if err := e.CreateBucketDir("vault"); err != nil {
				t.Fatalf("CreateBucketDir: %v", err)
			}

			// Marker first, then a child under it (the exact failing sequence).
			if _, _, err := e.PutObject("vault", "attachments/", strings.NewReader(""), 0); err != nil {
				t.Fatalf("put marker: %v", err)
			}
			if _, _, err := e.PutObject("vault", "attachments/ccd/file.txt", strings.NewReader("hello"), 5); err != nil {
				t.Fatalf("put child under marker: %v", err)
			}

			// Child first, then its marker (order must not matter).
			if _, _, err := e.PutObject("vault", "logs/app.log", strings.NewReader("x"), 1); err != nil {
				t.Fatalf("put child first: %v", err)
			}
			if _, _, err := e.PutObject("vault", "logs/", strings.NewReader(""), 0); err != nil {
				t.Fatalf("put marker over existing dir: %v", err)
			}

			// Marker reads back as zero bytes (not "object is a directory").
			rc, n, err := e.GetObject("vault", "attachments/")
			if err != nil {
				t.Fatalf("get marker: %v", err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if n != 0 || len(b) != 0 {
				t.Fatalf("marker should be empty, got n=%d bytes=%q", n, b)
			}

			// Child content is intact.
			rc2, _, err := e.GetObject("vault", "attachments/ccd/file.txt")
			if err != nil {
				t.Fatalf("get child: %v", err)
			}
			cb, _ := io.ReadAll(rc2)
			rc2.Close()
			if string(cb) != "hello" {
				t.Fatalf("child = %q, want hello", cb)
			}

			// Deleting the marker while a child remains succeeds and keeps the child.
			if err := e.DeleteObject("vault", "attachments/"); err != nil {
				t.Fatalf("delete marker with child: %v", err)
			}
			if _, _, err := e.GetObject("vault", "attachments/ccd/file.txt"); err != nil {
				t.Fatalf("child gone after marker delete: %v", err)
			}
		})
	}
}
