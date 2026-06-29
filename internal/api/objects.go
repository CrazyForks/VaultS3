package api

import (
	"archive/zip"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// listObjects returns the latest objects for a bucket. Versioned (Enabled or
// Suspended) buckets store data under .vs/, invisible to the engine's
// filesystem walk, so the metadata store's latest-pointer index is used as the
// source of truth. Non-versioned buckets use the engine.
func (h *APIHandler) listObjects(bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectInfo, bool, error) {
	if v, _ := h.store.GetBucketVersioning(bucket); v == "Enabled" || v == "Suspended" {
		metas, truncated, err := h.store.ListLatestObjects(bucket, prefix, startAfter, maxKeys)
		if err != nil {
			return nil, false, err
		}
		objects := make([]storage.ObjectInfo, 0, len(metas))
		for _, m := range metas {
			objects = append(objects, storage.ObjectInfo{
				Key:          m.Key,
				Size:         m.Size,
				LastModified: m.LastModified,
				ETag:         m.ETag,
			})
		}
		return objects, truncated, nil
	}
	return h.engine.ListObjects(bucket, prefix, startAfter, maxKeys)
}

type objectListItem struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
	ContentType  string `json:"contentType"`
	IsPrefix     bool   `json:"isPrefix"` // true = "folder"
}

type objectListResponse struct {
	Objects   []objectListItem `json:"objects"`
	Truncated bool             `json:"truncated"`
	Prefix    string           `json:"prefix"`
}

type uploadResult struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

func (h *APIHandler) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil && v > 0 && v <= 1000 {
			maxKeys = v
		}
	}

	objects, truncated, err := h.listObjects(bucket, prefix, startAfter, maxKeys)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list objects")
		return
	}

	// Extract common prefixes (folders) and direct objects
	prefixSet := make(map[string]bool)
	var items []objectListItem

	for _, obj := range objects {
		// Get the part after the current prefix
		rel := strings.TrimPrefix(obj.Key, prefix)
		if idx := strings.Index(rel, "/"); idx >= 0 {
			// This object is inside a "subfolder"
			folder := prefix + rel[:idx+1]
			if !prefixSet[folder] {
				prefixSet[folder] = true
				items = append(items, objectListItem{
					Key:      folder,
					IsPrefix: true,
				})
			}
		} else {
			// Direct object at this level
			ct := ""
			if meta, err := h.store.GetObjectMeta(bucket, obj.Key); err == nil && meta != nil {
				ct = meta.ContentType
			}
			items = append(items, objectListItem{
				Key:          obj.Key,
				Size:         obj.Size,
				LastModified: time.Unix(obj.LastModified, 0).UTC().Format(time.RFC3339),
				ContentType:  ct,
			})
		}
	}

	if items == nil {
		items = []objectListItem{}
	}
	writeJSON(w, http.StatusOK, objectListResponse{
		Objects:   items,
		Truncated: truncated,
		Prefix:    prefix,
	})
}

func (h *APIHandler) handleDeleteObject(w http.ResponseWriter, _ *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil || meta.DeleteMarker {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}

	if versioning, _ := h.store.GetBucketVersioning(bucket); versioning == "Enabled" || meta.VersionID != "" {
		// Versioned bucket: write a delete marker instead of erasing data. The
		// object disappears from listings but its versions are kept, so it stays
		// snapshot/restore-able (S3 versioned-delete semantics).
		old := *meta
		old.IsLatest = false
		h.store.PutObjectVersion(old)

		dm := metadata.ObjectMeta{
			Bucket: bucket, Key: key, VersionID: genVersionID(),
			DeleteMarker: true, IsLatest: true, LastModified: time.Now().UTC().Unix(),
		}
		h.store.PutObjectVersion(dm)
		h.store.PutObjectMeta(dm)
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", dm.VersionID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Non-versioned: hard delete.
	if err := h.engine.DeleteObject(bucket, key); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete object")
		return
	}
	h.store.DeleteObjectMeta(bucket, key)
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// getLatestObject returns a reader for an object's current content, resolving the
// latest version when the bucket is versioned (data then lives under .vs/, not
// at the plain key path).
func (h *APIHandler) getLatestObject(bucket, key string) (io.ReadCloser, int64, *metadata.ObjectMeta, error) {
	meta, _ := h.store.GetObjectMeta(bucket, key)
	if meta != nil && meta.VersionID != "" {
		r, sz, err := h.engine.GetObjectVersion(bucket, key, meta.VersionID)
		return r, sz, meta, err
	}
	r, sz, err := h.engine.GetObject(bucket, key)
	return r, sz, meta, err
}

func (h *APIHandler) handleDownload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	reader, size, meta, err := h.getLatestObject(bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	defer reader.Close()

	// Set content type from metadata
	ct := "application/octet-stream"
	if meta != nil && meta.ContentType != "" {
		ct = meta.ContentType
	}

	// Extract filename from key
	filename := filepath.Base(key)

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	// Sanitize filename: remove quotes and control characters to prevent header injection
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
	io.Copy(w, reader)
}

func (h *APIHandler) handleUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	// Max 100MB per request
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse upload")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	versioning, _ := h.store.GetBucketVersioning(bucket)
	var results []uploadResult

	for _, fileHeaders := range r.MultipartForm.File {
		for _, fh := range fileHeaders {
			file, err := fh.Open()
			if err != nil {
				continue
			}

			key := prefix + fh.Filename
			if err := validateObjectKey(key); err != nil {
				file.Close()
				continue
			}

			// Detect content type up front.
			ct := fh.Header.Get("Content-Type")
			if ct == "" || ct == "application/octet-stream" {
				if detected := mime.TypeByExtension(filepath.Ext(key)); detected != "" {
					ct = detected
				} else {
					ct = "application/octet-stream"
				}
			}

			now := time.Now().UTC().Unix()
			var written int64
			var etag string

			if versioning == "Enabled" {
				// Versioned bucket: write a new version so the object has history
				// (and is snapshot/restore-able), mirroring the S3 PutObject path.
				versionID := genVersionID()
				written, etag, err = h.engine.PutObjectVersion(bucket, key, versionID, file, fh.Size)
				file.Close()
				if err != nil {
					continue
				}
				if old, e := h.store.GetObjectMeta(bucket, key); e == nil && old.VersionID != "" {
					old.IsLatest = false
					h.store.PutObjectVersion(*old)
				}
				meta := metadata.ObjectMeta{
					Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written,
					LastModified: now, VersionID: versionID, IsLatest: true,
				}
				h.store.PutObjectVersion(meta)
				h.store.PutObjectMeta(meta)
				if h.onReplication != nil {
					h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, versionID)
				}
			} else {
				written, etag, err = h.engine.PutObject(bucket, key, file, fh.Size)
				file.Close()
				if err != nil {
					continue
				}
				h.store.PutObjectMeta(metadata.ObjectMeta{
					Bucket: bucket, Key: key, ContentType: ct, ETag: etag, Size: written, LastModified: now,
				})
				if h.onReplication != nil {
					h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, "")
				}
			}

			results = append(results, uploadResult{
				Key:         key,
				Size:        written,
				ContentType: ct,
			})
		}
	}

	if results == nil {
		results = []uploadResult{}
	}
	writeJSON(w, http.StatusOK, results)
}

// handleBulkDelete deletes multiple objects at once.
func (h *APIHandler) handleBulkDelete(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	var req struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "no keys provided")
		return
	}
	if len(req.Keys) > 1000 {
		writeError(w, http.StatusBadRequest, "max 1000 keys per request")
		return
	}

	type deleteResult struct {
		Key     string `json:"key"`
		Deleted bool   `json:"deleted"`
		Error   string `json:"error,omitempty"`
	}
	var results []deleteResult

	for _, key := range req.Keys {
		if !h.engine.ObjectExists(bucket, key) {
			results = append(results, deleteResult{Key: key, Error: "not found"})
			continue
		}
		if err := h.engine.DeleteObject(bucket, key); err != nil {
			results = append(results, deleteResult{Key: key, Error: err.Error()})
			continue
		}
		h.store.DeleteObjectMeta(bucket, key)
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
		}
		results = append(results, deleteResult{Key: key, Deleted: true})
	}

	writeJSON(w, http.StatusOK, results)
}

// handleDownloadZip streams multiple objects as a zip archive.
func (h *APIHandler) handleDownloadZip(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	keysParam := r.URL.Query().Get("keys")
	if keysParam == "" {
		writeError(w, http.StatusBadRequest, "no keys provided")
		return
	}
	keys := strings.Split(keysParam, ",")
	if len(keys) > 1000 {
		writeError(w, http.StatusBadRequest, "max 1000 keys per request")
		return
	}

	// Sanitize bucket name for Content-Disposition header
	safeBucket := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, bucket)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-files.zip"`, safeBucket))

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, key := range keys {
		// Validate key to prevent zip slip
		if err := validateObjectKey(key); err != nil {
			continue
		}
		reader, _, _, err := h.getLatestObject(bucket, key)
		if err != nil {
			continue
		}
		fw, err := zw.Create(key)
		if err != nil {
			reader.Close()
			continue
		}
		io.Copy(fw, reader)
		reader.Close()
	}
}
