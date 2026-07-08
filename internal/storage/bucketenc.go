package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"sync"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// PerBucketEngine encrypts/decrypts objects with a per-bucket data key resolved
// from a bucketcrypto.Manager (see docs/design/per-bucket-encryption.md, phase 3).
// Buckets without a key are stored as plaintext (opt-out). Objects written before
// per-bucket mode — which lack the per-bucket header — are read with an optional
// legacy global key, or as plaintext when none is configured.
//
// All non-crypto Engine methods delegate to the embedded inner Engine.
type PerBucketEngine struct {
	Engine              // inner engine; promoted methods delegate by default
	mu     sync.RWMutex // guards mgr (set after construction, before serving)
	mgr    *bucketcrypto.Manager
	legacy cipher.AEAD // optional: decrypt legacy global-key objects
}

// NewPerBucketEngine wraps inner. legacyKey (32 bytes) is optional and only used
// to read objects written by the old server-wide encryption.
func NewPerBucketEngine(inner Engine, legacyKey []byte) (*PerBucketEngine, error) {
	pe := &PerBucketEngine{Engine: inner}
	if len(legacyKey) > 0 {
		if len(legacyKey) != 32 {
			return nil, fmt.Errorf("legacy key must be 32 bytes, got %d", len(legacyKey))
		}
		block, err := aes.NewCipher(legacyKey)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		pe.legacy = gcm
	}
	return pe, nil
}

// SetManager wires the per-bucket key manager. Until set, every bucket is treated
// as opted-out (plaintext).
func (e *PerBucketEngine) SetManager(m *bucketcrypto.Manager) {
	e.mu.Lock()
	e.mgr = m
	e.mu.Unlock()
}

func (e *PerBucketEngine) manager() *bucketcrypto.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mgr
}

// seal encrypts plaintext for a bucket (or returns it unchanged when the bucket
// is opted out / no manager is set).
func (e *PerBucketEngine) seal(bucket string, plaintext []byte) ([]byte, error) {
	m := e.manager()
	if m == nil {
		return plaintext, nil
	}
	out, _, err := m.Encrypt(bucket, plaintext)
	return out, err
}

// open reverses seal, picking the scheme from the blob: per-bucket header → the
// bucket's key; else the legacy global key (if configured); else plaintext.
func (e *PerBucketEngine) open(bucket string, data []byte) ([]byte, error) {
	if m := e.manager(); m != nil && bucketcrypto.HasHeader(data) {
		return m.Decrypt(bucket, data)
	}
	if e.legacy != nil {
		ns := e.legacy.NonceSize()
		if len(data) < ns {
			return nil, fmt.Errorf("encrypted data too short")
		}
		return e.legacy.Open(nil, data[:ns], data[ns:], nil)
	}
	return data, nil
}

func (e *PerBucketEngine) readAll(r ReadSeekCloser) ([]byte, error) {
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxEncryptedSize+int64(64)))
}

func (e *PerBucketEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) {
		return e.Engine.PutObject(bucket, key, reader, size)
	}
	if size > maxEncryptedSize {
		return 0, "", fmt.Errorf("object too large for encryption (max %dMB)", maxEncryptedSize/(1024*1024))
	}
	plaintext, err := io.ReadAll(io.LimitReader(reader, maxEncryptedSize+1))
	if err != nil {
		return 0, "", fmt.Errorf("read plaintext: %w", err)
	}
	data, err := e.seal(bucket, plaintext)
	if err != nil {
		return 0, "", fmt.Errorf("encrypt: %w", err)
	}
	_, etag, err := e.Engine.PutObject(bucket, key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, "", err
	}
	return int64(len(plaintext)), etag, nil
}

func (e *PerBucketEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) {
		return e.Engine.GetObject(bucket, key)
	}
	reader, _, err := e.Engine.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	data, err := e.readAll(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("read object: %w", err)
	}
	plain, err := e.open(bucket, data)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt: %w", err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plain)}, int64(len(plain)), nil
}

func (e *PerBucketEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	if size > maxEncryptedSize {
		return 0, "", fmt.Errorf("object too large for encryption (max %dMB)", maxEncryptedSize/(1024*1024))
	}
	plaintext, err := io.ReadAll(io.LimitReader(reader, maxEncryptedSize+1))
	if err != nil {
		return 0, "", fmt.Errorf("read plaintext: %w", err)
	}
	data, err := e.seal(bucket, plaintext)
	if err != nil {
		return 0, "", fmt.Errorf("encrypt: %w", err)
	}
	_, etag, err := e.Engine.PutObjectVersion(bucket, key, versionID, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, "", err
	}
	return int64(len(plaintext)), etag, nil
}

func (e *PerBucketEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	reader, _, err := e.Engine.GetObjectVersion(bucket, key, versionID)
	if err != nil {
		return nil, 0, err
	}
	data, err := e.readAll(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("read object: %w", err)
	}
	plain, err := e.open(bucket, data)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt: %w", err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plain)}, int64(len(plain)), nil
}
