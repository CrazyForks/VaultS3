package migrate

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Job tracks the progress of one migration.
type Job struct {
	ID         string   `json:"id"`
	Endpoint   string   `json:"endpoint"`
	Buckets    []string `json:"buckets"`
	Status     string   `json:"status"` // "running", "completed", "failed"
	Total      int      `json:"total"`
	Copied     int      `json:"copied"`
	Failed     int      `json:"failed"`
	Error      string   `json:"error,omitempty"`
	StartedAt  int64    `json:"started_at"`
	FinishedAt int64    `json:"finished_at,omitempty"`
}

// Manager runs migrations from S3-compatible sources into the local store/engine.
type Manager struct {
	store  *metadata.Store
	engine storage.Engine
	mu     sync.RWMutex
	jobs   map[string]*Job
	seq    int
}

// NewManager creates a migration manager.
func NewManager(store *metadata.Store, engine storage.Engine) *Manager {
	return &Manager{store: store, engine: engine, jobs: make(map[string]*Job)}
}

// StartConfig describes a migration request.
type StartConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Region    string
	Buckets   []string // empty = all source buckets
}

// TestConnection validates credentials by listing the source buckets.
func (m *Manager) TestConnection(cfg StartConfig) ([]string, error) {
	return NewSource(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Region, 30).ListBuckets()
}

// Start validates the source then launches an async migration; returns the job ID.
func (m *Manager) Start(cfg StartConfig) (string, error) {
	src := NewSource(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Region, 300)

	buckets := cfg.Buckets
	if len(buckets) == 0 {
		all, err := src.ListBuckets()
		if err != nil {
			return "", fmt.Errorf("list source buckets: %w", err)
		}
		buckets = all
	}
	if len(buckets) == 0 {
		return "", fmt.Errorf("no buckets to migrate")
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("migrate-%d", m.seq)
	job := &Job{
		ID:        id,
		Endpoint:  cfg.Endpoint,
		Buckets:   buckets,
		Status:    "running",
		StartedAt: time.Now().Unix(),
	}
	m.jobs[id] = job
	m.mu.Unlock()

	go m.run(src, job)
	return id, nil
}

func (m *Manager) run(src *Source, job *Job) {
	for _, bucket := range job.Buckets {
		if !m.store.BucketExists(bucket) {
			if err := m.store.CreateBucket(bucket); err != nil {
				m.setError(job, fmt.Sprintf("create bucket %s: %v", bucket, err))
				return
			}
		}
		m.engine.CreateBucketDir(bucket)

		token := ""
		for {
			objs, next, err := src.ListObjects(bucket, token)
			if err != nil {
				m.setError(job, fmt.Sprintf("list %s: %v", bucket, err))
				return
			}
			m.bump(job, func(j *Job) { j.Total += len(objs) })

			for _, o := range objs {
				if err := m.copyOne(src, bucket, o); err != nil {
					slog.Warn("migrate: copy failed", "bucket", bucket, "key", o.Key, "error", err)
					m.bump(job, func(j *Job) { j.Failed++ })
					continue
				}
				m.bump(job, func(j *Job) { j.Copied++ })
			}
			if next == "" {
				break
			}
			token = next
		}
	}
	m.bump(job, func(j *Job) {
		if j.Status == "running" {
			j.Status = "completed"
		}
		j.FinishedAt = time.Now().Unix()
	})
	slog.Info("migrate: completed", "id", job.ID, "copied", job.Copied, "failed", job.Failed)
}

func (m *Manager) copyOne(src *Source, bucket string, o ObjectInfo) error {
	body, ct, err := src.GetObject(bucket, o.Key)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return err
	}
	written, etag, err := m.engine.PutObject(bucket, o.Key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return m.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:       bucket,
		Key:          o.Key,
		ContentType:  ct,
		ETag:         etag,
		Size:         written,
		LastModified: time.Now().Unix(),
	})
}

func (m *Manager) bump(job *Job, fn func(*Job)) {
	m.mu.Lock()
	fn(job)
	m.mu.Unlock()
}

func (m *Manager) setError(job *Job, msg string) {
	m.bump(job, func(j *Job) {
		j.Status = "failed"
		j.Error = msg
		j.FinishedAt = time.Now().Unix()
	})
	slog.Error("migrate: failed", "id", job.ID, "error", msg)
}

// GetJob returns a snapshot copy of a job (safe to read while it runs).
func (m *Manager) GetJob(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j := m.jobs[id]
	if j == nil {
		return nil
	}
	cp := *j
	return &cp
}

// ListJobs returns snapshot copies of all jobs.
func (m *Manager) ListJobs() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		cp := *j
		out = append(out, &cp)
	}
	return out
}
