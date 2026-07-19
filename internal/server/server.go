package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"net/http/pprof"
	"runtime/debug"

	"github.com/Kodiqa-Solutions/VaultS3/internal/accesslog"
	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketkeys"
	"github.com/Kodiqa-Solutions/VaultS3/internal/cluster"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/dashboard"
	"github.com/Kodiqa-Solutions/VaultS3/internal/erasure"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lifecycle"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/notify"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
	"github.com/Kodiqa-Solutions/VaultS3/internal/selfupdate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/snapshot"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
	"github.com/Kodiqa-Solutions/VaultS3/internal/vector"
)

// Version is the running build version, set by main from the -ldflags value.
var Version = "dev"

// reapClient issues the best-effort inter-node object-delete broadcasts (issue
// #34 layer 2). Inter-node TLS is commonly self-signed, so verification is
// skipped for these internal, cluster-secret-authenticated calls.
var reapClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// replClient streams object data to replica-set peers (issue #37, replica_count >
// 1). No overall timeout — it caps the whole request including the body, which
// would break large objects — only dial/response-header bounds.
var replClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	},
}

// clusterControllerAdapter adapts *cluster.Node to api.ClusterController so the
// admin API can drive membership without importing internal/cluster's raft types.
type clusterControllerAdapter struct{ n *cluster.Node }

func (a clusterControllerAdapter) SelfID() string             { return a.n.NodeID() }
func (a clusterControllerAdapter) IsLeader() bool             { return a.n.IsLeader() }
func (a clusterControllerAdapter) LeaderID() string           { return a.n.LeaderID() }
func (a clusterControllerAdapter) Join(id, addr string) error { return a.n.Join(id, addr) }
func (a clusterControllerAdapter) Leave(id string) error      { return a.n.Leave(id) }

func (a clusterControllerAdapter) Members() []api.ClusterMember {
	leaderID := a.n.LeaderID()
	members := a.n.MembersInfo()
	out := make([]api.ClusterMember, 0, len(members))
	for _, m := range members {
		out = append(out, api.ClusterMember{
			NodeID:   m.ID,
			Address:  m.Address,
			Suffrage: m.Suffrage,
			Leader:   m.ID == leaderID,
		})
	}
	return out
}

type Server struct {
	cfg             *config.Config
	store           *metadata.Store
	metaStore       metadata.StoreAPI
	engine          storage.Engine
	keyMgr          *bucketcrypto.Manager
	s3h             *s3.Handler
	metrics         *metrics.Collector
	activity        *api.ActivityLog
	accessLog       *accesslog.AccessLogger
	notifyDisp      *notify.Dispatcher
	replWorker      *replication.Worker
	biDirWorker     *replication.BiDirectionalWorker
	replicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)
	searchIndex     *search.Index
	vectorMgr       *vector.Manager
	scanWorker      *scanner.Scanner
	tieringMgr      *tiering.Manager
	backupSched     *backup.Scheduler
	rateLimiter     *ratelimit.Limiter
	lambdaMgr       *lambda.TriggerManager
	accessUpdater   *metadata.AccessUpdater
	clusterNode     *cluster.Node
	clusterProxy    *cluster.Proxy
	failoverProxy   *cluster.FailoverProxy
	failureDetector *cluster.FailureDetector
	rebalancer      *cluster.Rebalancer
	ecHealer        *erasure.Healer
	s3Auth          *s3.Authenticator
	writable        *atomic.Bool // node-local write gate shared by the S3 + admin handlers (drain)
}

func New(cfg *config.Config) (*Server, error) {
	// Initialize storage engine
	fs, err := storage.NewFileSystem(cfg.Storage.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	var engine storage.Engine = fs
	var perBucketEngine *storage.PerBucketEngine

	// Wrap with compression if enabled (compress before encrypt)
	if cfg.Compression.Enabled {
		engine = storage.NewCompressedEngine(engine)
		slog.Info("compression enabled", "algorithm", "gzip")
	}

	// Wrap with encryption if enabled (SSE-S3 or SSE-KMS)
	if cfg.Encryption.Enabled {
		if cfg.Encryption.PerBucket {
			// Per-bucket encryption: the configured key is the master KEK; objects are
			// encrypted with a per-bucket data key (provisioned on opt-in). The manager
			// is wired after the metadata store is ready.
			if _, err := cfg.Encryption.KeyBytes(); err != nil {
				return nil, fmt.Errorf("per-bucket encryption needs a valid master key: %w", err)
			}
			legacy, err := cfg.Encryption.LegacyKeyBytes()
			if err != nil {
				return nil, fmt.Errorf("encryption config: %w", err)
			}
			pe, err := storage.NewPerBucketEngine(engine, legacy)
			if err != nil {
				return nil, fmt.Errorf("init per-bucket encryption: %w", err)
			}
			engine = pe
			perBucketEngine = pe
			slog.Info("per-bucket encryption enabled (per-bucket keys, opt-in via PUT ?encryption)")
		} else if cfg.Encryption.KMS.Enabled {
			// SSE-KMS: use KMS for key management
			kms := storage.NewKMS(storage.KMSConfig{
				Provider:   cfg.Encryption.KMS.Provider,
				VaultAddr:  cfg.Encryption.KMS.VaultAddr,
				VaultToken: cfg.Encryption.KMS.VaultToken,
				KeyName:    cfg.Encryption.KMS.KeyName,
				LocalKey:   cfg.Encryption.KMS.LocalKey,
			})
			keyName := cfg.Encryption.KMS.KeyName
			if keyName == "" {
				keyName = "vaults3-default"
			}
			enc, err := storage.NewKMSEncryptedEngine(engine, kms, keyName)
			if err != nil {
				return nil, fmt.Errorf("init KMS encryption: %w", err)
			}
			engine = enc
			slog.Info("SSE-KMS encryption enabled", "provider", cfg.Encryption.KMS.Provider, "key", keyName)
		} else {
			// SSE-S3: static key
			keyBytes, err := cfg.Encryption.KeyBytes()
			if err != nil {
				return nil, fmt.Errorf("encryption config: %w", err)
			}
			enc, err := storage.NewEncryptedEngine(engine, keyBytes)
			if err != nil {
				return nil, fmt.Errorf("init encryption: %w", err)
			}
			engine = enc
			slog.Info("SSE-S3 encryption enabled", "algorithm", "AES-256-GCM")
		}
	}

	// Wrap with erasure coding if enabled
	var ecEngine *erasure.Engine
	var ecHealer *erasure.Healer
	if cfg.Erasure.Enabled {
		ec, err := erasure.NewEngine(engine, cfg.Erasure)
		if err != nil {
			return nil, fmt.Errorf("init erasure coding: %w", err)
		}
		ecEngine = ec
		engine = ec
		slog.Info("erasure coding enabled",
			"data_shards", cfg.Erasure.DataShards,
			"parity_shards", cfg.Erasure.ParityShards,
			"block_size", cfg.Erasure.BlockSize,
			"extra_dirs", len(cfg.Erasure.DataDirs),
		)
	}

	// Wrap with small-file packing if enabled (experimental). Small objects are
	// packed as zstd frames into large volume files; large objects fall through to
	// the layers below. Packed frames bypass the encryption/erasure layers, so for
	// now packing is mutually exclusive with them.
	if cfg.Packing.Enabled {
		if cfg.Encryption.Enabled || cfg.Erasure.Enabled {
			slog.Warn("small-file packing disabled: it does not yet compose with encryption or erasure coding")
		} else {
			pe, err := storage.NewPackedEngine(engine, cfg.Packing.MaxObjectSize, cfg.Packing.VolumeMaxSize)
			if err != nil {
				return nil, fmt.Errorf("init packing: %w", err)
			}
			engine = pe
			slog.Info("small-file packing enabled",
				"max_object_size", cfg.Packing.MaxObjectSize,
				"volume_max_size", cfg.Packing.VolumeMaxSize,
			)
			if cfg.Packing.CompactIntervalHours > 0 {
				ratio := cfg.Packing.CompactMinDeadRatio
				if ratio <= 0 {
					ratio = 0.5
				}
				interval := time.Duration(cfg.Packing.CompactIntervalHours) * time.Hour
				go func() {
					t := time.NewTicker(interval)
					defer t.Stop()
					for range t.C {
						if n, err := pe.Compact(ratio); err != nil {
							slog.Error("pack compaction failed", "error", err)
						} else if n > 0 {
							slog.Info("pack compaction reclaimed space", "bytes", n)
						}
					}
				}()
			}
		}
	}

	// Initialize metadata store
	metaDir := cfg.Storage.MetadataDir
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return nil, fmt.Errorf("create metadata dir: %w", err)
	}
	store, err := metadata.NewStore(filepath.Join(metaDir, "vaults3.db"))
	if err != nil {
		return nil, fmt.Errorf("init metadata: %w", err)
	}

	// Initialize erasure healer if EC is enabled
	if ecEngine != nil {
		healInterval := cfg.Erasure.HealInterval
		if healInterval <= 0 {
			healInterval = 3600
		}
		ecHealer = erasure.NewHealer(store, ecEngine, healInterval)
	}

	// Initialize cluster if enabled
	var clusterNode *cluster.Node
	var clusterProxy *cluster.Proxy
	if cfg.Cluster.Enabled {
		node, err := cluster.NewNode(cfg.Cluster, store)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init cluster: %w", err)
		}
		clusterNode = node

		// Build hash ring from configured peers + self
		vnodes := cfg.Cluster.Placement.VirtualNodes
		if vnodes <= 0 {
			vnodes = 128
		}
		ring := cluster.NewHashRing(vnodes)
		ring.AddNode(cfg.Cluster.NodeID)

		// Build peer API address map
		nodeAddrs := make(map[string]string)
		apiPort := cfg.Cluster.APIPort
		if apiPort == 0 {
			apiPort = cfg.Server.Port
		}
		nodeAddrs[cfg.Cluster.NodeID] = fmt.Sprintf("%s:%d", cfg.Cluster.BindAddr, apiPort)

		// Add peers to ring and address map
		for _, peer := range cfg.Cluster.Peers {
			nodeID, _, ok := cluster.ParsePeer(peer)
			if !ok {
				continue
			}
			ring.AddNode(nodeID)
		}
		// Explicit peer API addresses override auto-derived ones
		for nodeID, addr := range cfg.Cluster.PeerAPIs {
			nodeAddrs[nodeID] = addr
			if !ring.HasNode(nodeID) {
				ring.AddNode(nodeID)
			}
		}

		clusterProxy = cluster.NewProxy(ring, node, cfg.Cluster.Placement, nodeAddrs)
		slog.Info("cluster mode enabled",
			"node_id", cfg.Cluster.NodeID,
			"ring_nodes", ring.NodeCount(),
			"replica_count", cfg.Cluster.Placement.ReplicaCount,
		)
	}

	// Initialize failure detector and failover proxy if cluster is enabled
	var failureDetector *cluster.FailureDetector
	var failoverProxy *cluster.FailoverProxy
	var rebalancer *cluster.Rebalancer
	if clusterNode != nil && clusterProxy != nil {
		// Failure detector
		failureDetector = cluster.NewFailureDetector(cfg.Cluster.NodeID, cfg.Cluster.Detector)
		for nodeID, addr := range clusterProxy.NodeAddrs() {
			failureDetector.AddNode(nodeID, addr)
		}

		// Failover proxy wraps the basic proxy with failure awareness
		failoverProxy = cluster.NewFailoverProxy(clusterProxy, failureDetector)

		// Wire callbacks: node down/recover → failover + rebalance
		rebalancer = cluster.NewRebalancer(store, engine, clusterProxy.Ring(), clusterProxy, cfg.Cluster.NodeID, cfg.Cluster.Rebalance)
		failureDetector.SetCallbacks(
			func(nodeID string) {
				failoverProxy.OnNodeDown(nodeID)
				rebalancer.Trigger()
			},
			func(nodeID string) {
				failoverProxy.OnNodeRecover(nodeID)
				rebalancer.Trigger()
			},
		)
	}

	// Initialize S3 authenticator
	auth := s3.NewAuthenticator(cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey, store,
		cfg.Security.IPAllowlist, cfg.Security.IPBlocklist)
	// Behind a reverse-proxy subpath, the client signs the URI with the prefix that
	// the proxy strips before we see it, so SigV4 verification must add it back to
	// match (issue #36). Reuses server.base_path.
	auth.SetBasePath(cfg.Server.BasePath)

	// Load persisted admin credentials (overrides config/env if previously changed via dashboard)
	if ak, sk, err := store.GetAdminCredentials(); err == nil && ak != "" && sk != "" {
		cfg.Auth.AdminAccessKey = ak
		cfg.Auth.AdminSecretKey = sk
		auth.UpdateAdminCredentials(ak, sk)
		slog.Info("loaded persisted admin credentials")
	}

	// Initialize metrics collector
	mc := metrics.NewCollector(store, engine)

	// Initialize activity log
	activityLog := api.NewActivityLog()

	// When clustered, route metadata WRITES through Raft consensus so every node
	// converges; reads stay local. Single-node uses the store directly. Handlers
	// depend on the metadata.StoreAPI interface, which both satisfy.
	var metaStore metadata.StoreAPI = store
	if clusterNode != nil {
		metaStore = metadata.NewDistributedStore(store, clusterNode)
		slog.Info("cluster: metadata writes routed through Raft consensus")
	}

	// Initialize S3 handler
	s3h := s3.NewHandler(metaStore, engine, auth, cfg.Encryption.Enabled, cfg.Server.Domain, mc)

	// Node write gate (drain): shared by the S3 handler (rejects object writes when
	// draining) and the admin API (toggles it). Starts writable.
	writable := &atomic.Bool{}
	writable.Store(true)
	s3h.SetWritableFlag(writable)

	// Keep in-progress multipart upload metadata on the node-local store, not Raft.
	// All requests for an object route to the same owner node and its parts live on
	// that node's local disk, so replicating the metadata only added a
	// read-after-write lag that 404'd concurrent part uploads (issue #32). `store`
	// is the raw local store even when metaStore is the distributed one.
	s3h.SetLocalMultipartStore(store)

	// Cluster delete-reaper: after a delete, remove the object's data file from
	// every other node so an orphan copy left by a past ring/primary change doesn't
	// linger on disk (issue #34 layer 2). Best-effort + async — correctness already
	// comes from metadata being authoritative, this only reclaims disk.
	if clusterProxy != nil {
		reapSecret := cfg.Cluster.Secret
		reapSelf := cfg.Cluster.NodeID
		reapScheme := "http"
		if cfg.Server.TLS.Enabled {
			reapScheme = "https"
		}
		s3h.SetReplicaReaper(func(bucket, key string) {
			for id, addr := range clusterProxy.NodeAddrs() {
				if id == reapSelf || addr == "" {
					continue
				}
				u := reapScheme + "://" + addr + "/cluster/object-delete?bucket=" +
					url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
				go func(u string) {
					req, err := http.NewRequest(http.MethodPost, u, nil)
					if err != nil {
						return
					}
					if reapSecret != "" {
						req.Header.Set("X-Cluster-Secret", reapSecret)
					}
					if resp, err := reapClient.Do(req); err == nil {
						resp.Body.Close()
					}
				}(u)
			}
		})

		// replica_count > 1: after a write, stream the object's data to the other
		// nodes in its replica set so a node loss doesn't make it unavailable (issue
		// #37). Best-effort + async — never blocks/fails the client write; GET
		// failover already tries replicas. Each peer is streamed from the engine
		// (no whole-object buffering).
		if cfg.Cluster.Placement.ReplicaCount > 1 {
			repSecret := cfg.Cluster.Secret
			repSelf := cfg.Cluster.NodeID
			repScheme := reapScheme
			repCount := cfg.Cluster.Placement.ReplicaCount
			localEngine := engine
			ring := clusterProxy.Ring()
			s3h.SetPlacementReplicator(func(bucket, key string) {
				addrs := clusterProxy.NodeAddrs()
				for _, id := range ring.GetNodes(bucket, key, repCount) {
					if id == repSelf {
						continue
					}
					addr := addrs[id]
					if addr == "" {
						continue
					}
					go func(addr string) {
						reader, size, err := localEngine.GetObject(bucket, key)
						if err != nil {
							return
						}
						defer reader.Close()
						u := repScheme + "://" + addr + "/cluster/replica-put?bucket=" +
							url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
						req, err := http.NewRequest(http.MethodPost, u, reader)
						if err != nil {
							return
						}
						req.ContentLength = size
						if repSecret != "" {
							req.Header.Set("X-Cluster-Secret", repSecret)
						}
						if resp, err := replClient.Do(req); err == nil {
							resp.Body.Close()
						}
					}(addr)
				}
			})
		}
	}

	// Per-bucket encryption keys: when a master key is configured, opting a bucket
	// into SSE-S3 provisions a per-bucket data key (see
	// docs/design/per-bucket-encryption.md). Reuses the encryption master key as KEK.
	var keyMgr *bucketcrypto.Manager
	if mk, err := cfg.Encryption.KeyBytes(); err == nil && len(mk) == 32 {
		if km, kerr := bucketkeys.NewManager(metaStore, mk); kerr == nil {
			keyMgr = km
			s3h.SetKeyManager(keyMgr)
			if perBucketEngine != nil {
				perBucketEngine.SetManager(keyMgr) // activate per-bucket crypto in the data path
			}
			slog.Info("per-bucket encryption key management enabled")
		}
	}

	// Wire cluster proxy into S3 handler (use failover proxy if available)
	if failoverProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			return failoverProxy.ForwardWithRetry(w, r, bucket, key)
		})
	} else if clusterProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			targetNode := clusterProxy.ShouldProxy(bucket, key)
			if targetNode == "" {
				return false
			}
			clusterProxy.ForwardRequest(w, r, targetNode)
			return true
		})
	}

	// Initialize access logger if enabled
	var accessLogger *accesslog.AccessLogger
	if cfg.Logging.Enabled {
		var err error
		accessLogger, err = accesslog.NewAccessLogger(cfg.Logging.FilePath)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init access logger: %w", err)
		}
		slog.Info("access logging enabled", "path", cfg.Logging.FilePath)
	}

	// Wire activity recording from S3 handler to activity log + access logger
	s3h.SetActivityFunc(func(method, bucket, key string, status int, size int64, clientIP string) {
		// Skip browser noise
		if bucket == "favicon.ico" {
			return
		}
		now := time.Now().UTC()
		activityLog.Record(api.ActivityEntry{
			Time:     now,
			Method:   method,
			Bucket:   bucket,
			Key:      key,
			Status:   status,
			Size:     size,
			ClientIP: clientIP,
		})
		if accessLogger != nil {
			accessLogger.Log(accesslog.AccessEntry{
				Time:     now,
				Method:   method,
				Bucket:   bucket,
				Key:      key,
				Status:   status,
				Bytes:    size,
				ClientIP: clientIP,
			})
		}
	})

	// Wire audit trail recording
	s3h.SetAuditFunc(func(principal, userID, action, resource, effect, sourceIP string, statusCode int) {
		store.PutAuditEntry(metadata.AuditEntry{
			Time:       time.Now().UnixNano(),
			Principal:  principal,
			UserID:     userID,
			Action:     action,
			Resource:   resource,
			Effect:     effect,
			SourceIP:   sourceIP,
			StatusCode: statusCode,
		})
	})

	// Initialize notification dispatcher
	nc := cfg.Notifications
	notifyDispatcher := notify.NewDispatcher(store, nc.MaxWorkers, nc.QueueSize, nc.TimeoutSecs, nc.MaxRetries)

	// Register notification backends
	if nc.Kafka.Enabled && len(nc.Kafka.Brokers) > 0 && nc.Kafka.Topic != "" {
		notifyDispatcher.AddBackend(notify.NewKafkaBackend(nc.Kafka.Brokers, nc.Kafka.Topic))
	}
	if nc.NATS.Enabled && nc.NATS.URL != "" && nc.NATS.Subject != "" {
		natsBackend, err := notify.NewNATSBackend(nc.NATS.URL, nc.NATS.Subject)
		if err != nil {
			slog.Warn("NATS backend failed to connect", "error", err)
		} else {
			notifyDispatcher.AddBackend(natsBackend)
		}
	}
	if nc.Redis.Enabled && nc.Redis.Addr != "" {
		notifyDispatcher.AddBackend(notify.NewRedisBackend(nc.Redis.Addr, nc.Redis.Channel, nc.Redis.ListKey))
	}
	if nc.AMQP.Enabled && nc.AMQP.URL != "" {
		notifyDispatcher.AddBackend(notify.NewAMQPBackend(nc.AMQP.URL, nc.AMQP.Exchange, nc.AMQP.RoutingKey))
	}
	if nc.Postgres.Enabled && nc.Postgres.ConnStr != "" {
		pgBackend, err := notify.NewPostgresBackend(nc.Postgres.ConnStr, nc.Postgres.Table)
		if err != nil {
			slog.Warn("PostgreSQL notification backend failed", "error", err)
		} else {
			notifyDispatcher.AddBackend(pgBackend)
		}
	}

	s3h.SetNotificationFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
		notifyDispatcher.Dispatch(bucket, key, eventType, size, etag, versionID)
	})

	// Initialize replication worker if enabled
	var replWorker *replication.Worker
	var biDirWorker *replication.BiDirectionalWorker
	// Shared so both the S3 handler and the dashboard API handler enqueue
	// replication events — dashboard uploads/deletes must replicate too (issue #10).
	var replicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)
	if cfg.Replication.Enabled && len(cfg.Replication.Peers) > 0 {
		// Register peer access keys so replication header is only trusted from peers
		var peerKeys []string
		for _, peer := range cfg.Replication.Peers {
			peerKeys = append(peerKeys, peer.AccessKey)
		}
		s3h.SetReplicationPeerKeys(peerKeys)

		if cfg.Replication.Mode == "active-active" {
			// Active-active bidirectional replication
			biDirWorker = replication.NewBiDirectionalWorker(store, engine, cfg.Replication)
			changeLog := biDirWorker.ChangeLog()
			siteID := biDirWorker.SiteID()
			replicationFunc = func(eventType, bucket, key string, size int64, etag, versionID string) {
				evtType := "put"
				if eventType == "s3:ObjectRemoved:Delete" {
					evtType = "delete"
				}
				vc := replication.NewVectorClock()
				vc.Increment(siteID)
				// Also store the vector clock on the object metadata
				if meta, err := store.GetObjectMeta(bucket, key); err == nil {
					existingVC, _ := replication.ParseVectorClock(meta.VectorClock)
					vc = existingVC.Merge(vc)
					vc.Increment(siteID)
					meta.VectorClock = vc.Bytes()
					store.PutObjectMeta(*meta)
				}
				changeLog.Record(bucket, key, evtType, etag, size, vc)
			}
			slog.Info("active-active replication enabled",
				"site_id", siteID,
				"peers", len(cfg.Replication.Peers),
				"conflict_strategy", cfg.Replication.ConflictStrategy,
			)
		} else {
			// Traditional push-based replication
			replWorker = replication.NewWorker(store, engine, cfg.Replication)
			replicationFunc = func(eventType, bucket, key string, size int64, etag, versionID string) {
				evtType := "put"
				if eventType == "s3:ObjectRemoved:Delete" {
					evtType = "delete"
				}
				for _, peer := range cfg.Replication.Peers {
					store.EnqueueReplication(metadata.ReplicationEvent{
						Type:   evtType,
						Bucket: bucket,
						Key:    key,
						ETag:   etag,
						Peer:   peer.Name,
						Size:   size,
					})
				}
			}
			slog.Info("push replication enabled", "peers", len(cfg.Replication.Peers), "interval_secs", cfg.Replication.ScanIntervalSecs)
		}
		s3h.SetReplicationFunc(replicationFunc)
	}

	// Build search index
	searchIdx := search.NewIndex(store, cfg.Memory.MaxSearchEntries)
	if err := searchIdx.Build(); err != nil {
		slog.Warn("search index build failed", "error", err)
	}

	// Optional vector / semantic-search add-on
	var vectorMgr *vector.Manager
	if cfg.Vector.Enabled && cfg.Vector.EmbeddingURL != "" {
		emb := vector.NewOpenAICompatEmbedder(cfg.Vector.EmbeddingURL, cfg.Vector.APIKey, cfg.Vector.Model, cfg.Vector.TimeoutSecs)
		vectorMgr = vector.NewManager(emb, vector.NewIndex(cfg.Vector.Dimensions, cfg.Vector.MaxVectors), cfg.Vector.PersistPath)
		slog.Info("vector search enabled", "model", cfg.Vector.Model, "auto_index", cfg.Vector.AutoIndex, "vectors", vectorMgr.Count())
	}

	s3h.SetSearchUpdateFunc(func(eventType, bucket, key string) {
		if eventType == "delete" {
			searchIdx.Remove(bucket, key)
			if vectorMgr != nil {
				vectorMgr.Remove(bucket, key)
			}
			return
		}
		meta, err := store.GetObjectMeta(bucket, key)
		if err != nil {
			return
		}
		searchIdx.Update(bucket, key, *meta)
		// Auto-index for vector search runs off the request path (embedding is a
		// network call) and is strictly best-effort.
		if vectorMgr != nil && cfg.Vector.AutoIndex && shouldVectorIndex(cfg.Vector, key, meta) {
			go indexObjectVector(vectorMgr, engine, bucket, key, cfg.Vector)
		}
	})

	// Initialize scanner if enabled
	var scanWorker *scanner.Scanner
	if cfg.Scanner.Enabled && cfg.Scanner.WebhookURL != "" {
		scanWorker = scanner.NewScanner(store, engine,
			cfg.Scanner.WebhookURL, cfg.Scanner.Workers,
			cfg.Scanner.TimeoutSecs, cfg.Scanner.QuarantineBucket,
			cfg.Scanner.FailClosed, cfg.Scanner.MaxScanSizeBytes, 256)
		s3h.SetScanFunc(func(bucket, key string, size int64) {
			scanWorker.Scan(bucket, key, size)
		})
	}

	// Initialize tiering if enabled
	var tieringMgr *tiering.Manager
	if cfg.Tiering.Enabled && cfg.Tiering.ColdDataDir != "" {
		coldFS, err := storage.NewFileSystem(cfg.Tiering.ColdDataDir)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init cold storage: %w", err)
		}
		tieringMgr = tiering.NewManager(store, fs, coldFS, cfg.Tiering.MigrateAfterDays, cfg.Tiering.ScanIntervalSecs)
		slog.Info("tiering enabled", "cold_dir", cfg.Tiering.ColdDataDir, "migrate_after_days", cfg.Tiering.MigrateAfterDays)
	}

	// Initialize backup scheduler if enabled
	var backupSched *backup.Scheduler
	if cfg.Backup.Enabled && len(cfg.Backup.Targets) > 0 {
		backupSched = backup.NewScheduler(store, engine, cfg.Backup)
		slog.Info("backup enabled", "targets", len(cfg.Backup.Targets), "schedule", cfg.Backup.ScheduleCron)
	}

	// Initialize rate limiter if enabled
	var rateLimiter *ratelimit.Limiter
	if cfg.RateLimit.Enabled {
		rateLimiter = ratelimit.NewLimiter(
			cfg.RateLimit.RequestsPerSec, cfg.RateLimit.BurstSize,
			cfg.RateLimit.PerKeyRPS, cfg.RateLimit.PerKeyBurst,
		)
		s3h.SetRateLimiter(rateLimiter)
		slog.Info("rate limiting enabled",
			"ip_rps", cfg.RateLimit.RequestsPerSec, "ip_burst", cfg.RateLimit.BurstSize,
			"key_rps", cfg.RateLimit.PerKeyRPS, "key_burst", cfg.RateLimit.PerKeyBurst)
	}

	// Initialize lambda trigger manager if enabled
	var lambdaMgr *lambda.TriggerManager
	if cfg.Lambda.Enabled {
		lambdaMgr = lambda.NewTriggerManager(store, engine, cfg.Lambda)
		s3h.SetLambdaFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
			lambdaMgr.Dispatch(bucket, key, eventType, size, etag, versionID)
		})
		slog.Info("lambda triggers enabled", "workers", cfg.Lambda.MaxWorkers, "queue_size", cfg.Lambda.QueueSize)
	}

	// Initialize batched access updater
	accessUpdater := metadata.NewAccessUpdater(store, 30*time.Second)
	s3h.SetAccessUpdater(accessUpdater)

	// Initialize built-in IAM policies
	initBuiltinPolicies(store)

	return &Server{
		cfg:             cfg,
		store:           store,
		metaStore:       metaStore,
		engine:          engine,
		keyMgr:          keyMgr,
		s3h:             s3h,
		metrics:         mc,
		activity:        activityLog,
		accessLog:       accessLogger,
		notifyDisp:      notifyDispatcher,
		replWorker:      replWorker,
		biDirWorker:     biDirWorker,
		replicationFunc: replicationFunc,
		searchIndex:     searchIdx,
		vectorMgr:       vectorMgr,
		scanWorker:      scanWorker,
		tieringMgr:      tieringMgr,
		backupSched:     backupSched,
		rateLimiter:     rateLimiter,
		lambdaMgr:       lambdaMgr,
		accessUpdater:   accessUpdater,
		clusterNode:     clusterNode,
		clusterProxy:    clusterProxy,
		failoverProxy:   failoverProxy,
		failureDetector: failureDetector,
		rebalancer:      rebalancer,
		writable:        writable,
		ecHealer:        ecHealer,
		s3Auth:          auth,
	}, nil
}

// Run starts the server and blocks until shutdown signal is received.
// It handles graceful shutdown with a configurable timeout.
func (s *Server) Run() error {
	addr := s.cfg.ListenAddr()

	// Dashboard API
	apiHandler := api.NewAPIHandler(s.metaStore, s.engine, s.metrics, s.cfg, s.activity)
	apiHandler.SetS3Authenticator(s.s3Auth)
	// Share the node write gate so the admin drain/undrain endpoints toggle the
	// same flag the S3 handler enforces.
	apiHandler.SetWritable(s.writable)
	apiHandler.SetSearchIndex(s.searchIndex)
	apiHandler.SetMigrator(migrate.NewManager(s.store, s.engine))
	apiHandler.SetSnapshotManager(snapshot.NewManager(s.store))
	// Per-bucket encryption controls (enable/rotate/shred) for the dashboard share
	// the SAME manager as the engine, so a shred evicts the live key cache too.
	if s.keyMgr != nil {
		apiHandler.SetKeyManager(s.keyMgr)
	}
	if s.replicationFunc != nil {
		apiHandler.SetReplicationFunc(s.replicationFunc)
	}
	// Cluster object routing: dashboard uploads place each file on its hash owner,
	// and downloads/deletes proxy to the owner — so dashboard data is consistent
	// with the S3 path across the cluster.
	if s.failoverProxy != nil && s.clusterProxy != nil {
		apiHandler.SetClusterRouting(
			func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
				return s.failoverProxy.ForwardWithRetry(w, r, bucket, key)
			},
			func(bucket, key string) (string, bool) {
				return s.clusterProxy.OwnerAPIAddr(bucket, key)
			},
		)
		// Cluster-wide capacity rollup: this node aggregates every node's
		// /api/v1/system for the mc-admin-info style view.
		apiHandler.SetClusterInfo(s.cfg.Cluster.NodeID, s.clusterProxy.NodeAddrs, s.cfg.Cluster.Secret)
	}
	// Cluster membership + rebalance operations for the admin API / vaults3-cli.
	if s.clusterNode != nil {
		apiHandler.SetClusterController(
			clusterControllerAdapter{n: s.clusterNode},
			func() {
				if s.rebalancer != nil {
					s.rebalancer.Trigger()
				}
			},
			func() bool { return s.rebalancer != nil && s.rebalancer.IsRunning() },
		)
	}

	// Update checker (notifier always; auto-apply only if explicitly enabled).
	updater := selfupdate.New(Version)
	apiHandler.SetUpdater(updater)
	if s.cfg.AutoUpdate.Enabled {
		go s.runUpdateChecker(updater)
	}
	if s.vectorMgr != nil {
		apiHandler.SetVectorManager(s.vectorMgr)
		// Persist the vector index periodically so embeddings survive restarts.
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for range t.C {
				if err := s.vectorMgr.Save(); err != nil {
					slog.Warn("vector: periodic save failed", "error", err)
				}
			}
		}()
	}
	if s.scanWorker != nil {
		apiHandler.SetScanner(s.scanWorker)
	}
	if s.tieringMgr != nil {
		apiHandler.SetTieringManager(s.tieringMgr)
	}
	if s.ecHealer != nil {
		apiHandler.SetHealer(s.ecHealer)
	}
	if s.backupSched != nil {
		apiHandler.SetBackupScheduler(s.backupSched)
	}
	if s.rateLimiter != nil {
		apiHandler.SetRateLimiter(s.rateLimiter)
	}

	// Wire OIDC validator if enabled
	if s.cfg.OIDC.Enabled && s.cfg.OIDC.IssuerURL != "" {
		if err := validateExternalURL(s.cfg.OIDC.IssuerURL); err != nil {
			slog.Warn("OIDC issuer URL rejected", "url", s.cfg.OIDC.IssuerURL, "error", err)
		} else {
			oidcValidator, err := api.NewOIDCValidator(
				s.cfg.OIDC.IssuerURL,
				s.cfg.OIDC.ClientID,
				s.cfg.OIDC.AllowedDomains,
				s.cfg.OIDC.JWKSCacheSecs,
			)
			if err != nil {
				slog.Warn("OIDC setup failed", "error", err)
			} else {
				apiHandler.SetOIDCValidator(oidcValidator)
				slog.Info("OIDC enabled", "issuer", s.cfg.OIDC.IssuerURL)
			}
		}
	}

	// When ConsolePort is set, the dashboard (Web UI) + its API move to a separate
	// listener (issue #18) so the S3 API and the console can have independent ports,
	// network rules, and TLS. Otherwise everything is served on the main port.
	splitConsole := s.cfg.Server.ConsolePort > 0 && s.cfg.Server.ConsolePort != s.cfg.Server.Port

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(s.metrics.StartTime()))
	mux.HandleFunc("/ready", readyHandler(s.store))
	mux.Handle("/metrics", s.metrics)
	if !splitConsole {
		mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/favicon.svg", http.StatusMovedPermanently)
		})
		mux.Handle("/api/v1/", apiHandler)
		mux.Handle("/dashboard/", dashboard.Handler(s.cfg.Server.BasePath))
	}

	// Register pprof endpoints when debug mode is enabled
	if s.cfg.Debug {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		slog.Info("pprof debug endpoints enabled at /debug/pprof/")
	}

	// Register cluster endpoints if enabled
	if s.clusterNode != nil {
		mux.HandleFunc("/cluster/status", s.clusterNode.StatusHandler())
		mux.HandleFunc("/cluster/sysinfo", apiHandler.ClusterSysInfoHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/readindex", s.clusterNode.ReadIndexHandler())
		mux.HandleFunc("/cluster/drain", apiHandler.ClusterDrainHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/object-delete", apiHandler.ClusterObjectDeleteHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/replica-put", apiHandler.ClusterReplicaPutHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		mux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		mux.HandleFunc("/cluster/apply", s.clusterNode.ApplyHandler())
		slog.Info("cluster endpoints registered", "paths", []string{"/cluster/status", "/cluster/sysinfo", "/cluster/join", "/cluster/leave", "/cluster/apply"})
	}

	// Register bidirectional replication sync endpoint
	if s.biDirWorker != nil {
		mux.HandleFunc("/_replication/sync", s.biDirWorker.HandleSyncRequest)
		slog.Info("bidirectional replication sync endpoint registered", "path", "/_replication/sync")
	}

	mux.Handle("/", s.s3h)

	// Wrap mux with middleware: panic recovery (outermost) → security headers → request ID → latency → mux
	var handler http.Handler = mux
	handler = middleware.Latency(s.metrics, handler)
	handler = middleware.RequestID(handler)
	handler = middleware.SecurityHeaders(handler)
	handler = middleware.PanicRecovery(handler)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Log startup info
	scheme := "http"
	if s.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	dashURL := fmt.Sprintf("%s://%s/dashboard/", scheme, addr)
	if splitConsole {
		caddr := s.cfg.Server.ConsoleAddress
		if caddr == "" {
			caddr = s.cfg.Server.Address
		}
		dashURL = fmt.Sprintf("%s://%s:%d/dashboard/", scheme, caddr, s.cfg.Server.ConsolePort)
	}
	slog.Info("VaultS3 starting",
		"addr", addr,
		"data_dir", s.cfg.Storage.DataDir,
		"metadata_dir", s.cfg.Storage.MetadataDir,
		"dashboard", dashURL,
	)
	if s.cfg.Auth.AdminAccessKey == "vaults3-admin" || s.cfg.Auth.AdminSecretKey == "vaults3-secret-change-me" {
		slog.Warn("Using default admin credentials. Set VAULTS3_ACCESS_KEY and VAULTS3_SECRET_KEY environment variables.")
	}
	if s.cfg.Encryption.Enabled {
		slog.Info("encryption enabled", "algorithm", "AES-256-GCM")
	}
	if s.cfg.Server.Domain != "" {
		slog.Info("virtual-hosted URLs enabled", "domain", s.cfg.Server.Domain)
	}
	if s.cfg.Server.TLS.Enabled {
		slog.Info("TLS enabled", "cert", s.cfg.Server.TLS.CertFile, "key", s.cfg.Server.TLS.KeyFile)
	}

	// Apply Go memory limit if configured
	if s.cfg.Memory.GoMemLimitMB > 0 {
		limit := int64(s.cfg.Memory.GoMemLimitMB) * 1024 * 1024
		debug.SetMemoryLimit(limit)
		slog.Info("memory limit set", "mb", s.cfg.Memory.GoMemLimitMB)
	}

	// Start batched access updater
	updaterCtx, updaterCancel := context.WithCancel(context.Background())
	defer updaterCancel()
	go s.accessUpdater.Run(updaterCtx)

	// Start lifecycle worker
	lcCtx, lcCancel := context.WithCancel(context.Background())
	defer lcCancel()
	lcWorker := lifecycle.NewWorker(s.store, s.engine, s.cfg.Lifecycle.ScanIntervalSecs, s.cfg.Security.AuditRetentionDays)
	go lcWorker.Run(lcCtx)
	slog.Info("lifecycle worker started", "interval_secs", s.cfg.Lifecycle.ScanIntervalSecs)

	// Start notification dispatcher
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	defer notifyCancel()
	s.notifyDisp.Start(notifyCtx)
	slog.Info("notifications started", "workers", s.cfg.Notifications.MaxWorkers, "queue_size", s.cfg.Notifications.QueueSize)

	// Start replication worker if enabled
	if s.replWorker != nil {
		replCtx, replCancel := context.WithCancel(context.Background())
		defer replCancel()
		go s.replWorker.Run(replCtx)
	}

	// Start bidirectional replication if enabled
	if s.biDirWorker != nil {
		biDirCtx, biDirCancel := context.WithCancel(context.Background())
		defer biDirCancel()
		go s.biDirWorker.Run(biDirCtx)
	}

	// Start scanner workers if enabled
	if s.scanWorker != nil {
		scanCtx, scanCancel := context.WithCancel(context.Background())
		defer scanCancel()
		s.scanWorker.Start(scanCtx, s.cfg.Scanner.Workers)
	}

	// Start tiering manager if enabled
	if s.tieringMgr != nil {
		tierCtx, tierCancel := context.WithCancel(context.Background())
		defer tierCancel()
		go s.tieringMgr.Run(tierCtx)
	}

	// Start lambda trigger manager if enabled
	if s.lambdaMgr != nil {
		lambdaCtx, lambdaCancel := context.WithCancel(context.Background())
		defer lambdaCancel()
		s.lambdaMgr.Start(lambdaCtx)
		apiHandler.SetLambdaManager(s.lambdaMgr)
	}

	// Start failure detector if cluster is enabled
	if s.failureDetector != nil {
		detCtx, detCancel := context.WithCancel(context.Background())
		defer detCancel()
		go s.failureDetector.Run(detCtx)
	}

	// Start erasure healer if enabled
	if s.ecHealer != nil {
		ecCtx, ecCancel := context.WithCancel(context.Background())
		defer ecCancel()
		go s.ecHealer.Run(ecCtx)
	}

	// Announce this node's current address to the cluster (every node, including
	// the bootstrap one). Runs in the background, retrying until the leader
	// accepts — so pod start order doesn't matter and a restart with a new pod IP
	// self-heals.
	if s.clusterNode != nil && s.cfg.Cluster.JoinAddr != "" {
		joinCtx, joinCancel := context.WithCancel(context.Background())
		defer joinCancel()
		go s.clusterNode.AutoJoin(joinCtx, s.cfg.Cluster.JoinAddr)
	}

	// Keep the data-placement ring in sync with live Raft membership. Without
	// this, auto-clustered nodes (which join dynamically, not via static config)
	// each see only themselves on the ring and place data inconsistently.
	if s.clusterProxy != nil {
		apiPort := s.cfg.Cluster.APIPort
		if apiPort == 0 {
			apiPort = s.cfg.Server.Port
		}
		syncCtx, syncCancel := context.WithCancel(context.Background())
		defer syncCancel()
		go s.clusterProxy.RunMembershipSync(syncCtx, apiPort)
	}

	// Start backup scheduler if enabled
	if s.backupSched != nil {
		backupCtx, backupCancel := context.WithCancel(context.Background())
		defer backupCancel()
		go s.backupSched.Run(backupCtx)
	}

	slog.Info("search index ready", "objects", s.searchIndex.Count())

	// Start separate inter-node listener if configured
	var interNodeServer *http.Server
	if s.cfg.Server.InterNodePort > 0 && s.clusterNode != nil {
		interNodeAddr := fmt.Sprintf("%s:%d", s.cfg.Server.InterNodeAddress, s.cfg.Server.InterNodePort)
		interNodeMux := http.NewServeMux()
		interNodeMux.HandleFunc("/cluster/status", s.clusterNode.StatusHandler())
		interNodeMux.HandleFunc("/cluster/sysinfo", apiHandler.ClusterSysInfoHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/readindex", s.clusterNode.ReadIndexHandler())
		interNodeMux.HandleFunc("/cluster/drain", apiHandler.ClusterDrainHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/object-delete", apiHandler.ClusterObjectDeleteHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/replica-put", apiHandler.ClusterReplicaPutHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		interNodeMux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		interNodeMux.HandleFunc("/cluster/apply", s.clusterNode.ApplyHandler())
		if s.biDirWorker != nil {
			interNodeMux.HandleFunc("/_replication/sync", s.biDirWorker.HandleSyncRequest)
		}
		interNodeServer = &http.Server{
			Addr:    interNodeAddr,
			Handler: interNodeMux,
		}
		go func() {
			slog.Info("inter-node listener started", "addr", interNodeAddr)
			if err := interNodeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("inter-node listener error", "error", err)
			}
		}()
	}

	// Start the separate console (dashboard) listener if configured (issue #18).
	var consoleServer *http.Server
	if splitConsole {
		caddr := s.cfg.Server.ConsoleAddress
		if caddr == "" {
			caddr = s.cfg.Server.Address
		}
		consoleAddr := fmt.Sprintf("%s:%d", caddr, s.cfg.Server.ConsolePort)
		cmux := http.NewServeMux()
		cmux.HandleFunc("/health", healthHandler(s.metrics.StartTime()))
		cmux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/favicon.svg", http.StatusMovedPermanently)
		})
		cmux.Handle("/api/v1/", apiHandler)
		cmux.Handle("/dashboard/", dashboard.Handler(s.cfg.Server.BasePath))
		cmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
		var chandler http.Handler = cmux
		chandler = middleware.RequestID(chandler)
		chandler = middleware.SecurityHeaders(chandler)
		chandler = middleware.PanicRecovery(chandler)
		consoleServer = &http.Server{Addr: consoleAddr, Handler: chandler}
		go func() {
			slog.Info("console (dashboard) listener started", "addr", consoleAddr)
			var err error
			if s.cfg.Server.TLS.Enabled {
				err = consoleServer.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
			} else {
				err = consoleServer.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				slog.Error("console listener error", "error", err)
			}
		}()
	}

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		if s.cfg.Server.TLS.Enabled {
			errCh <- httpServer.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()

	// Wait for signal or server error
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
	}

	// Graceful shutdown
	timeout := time.Duration(s.cfg.Server.ShutdownTimeoutSecs) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if interNodeServer != nil {
		interNodeServer.Shutdown(ctx)
	}
	if consoleServer != nil {
		consoleServer.Shutdown(ctx)
	}
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown timed out", "timeout", timeout, "error", err)
		return err
	}

	slog.Info("server stopped gracefully")
	return nil
}

func initBuiltinPolicies(store *metadata.Store) {
	builtins := []metadata.IAMPolicy{
		{
			Name:     "ReadOnlyAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket","s3:ListAllMyBuckets","s3:GetBucketPolicy"],"Resource":["*"]}]}`,
		},
		{
			Name:     "ReadWriteAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:ListBucket","s3:ListAllMyBuckets"],"Resource":["*"]}]}`,
		},
		{
			Name:     "FullAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`,
		},
	}

	for _, p := range builtins {
		p.CreatedAt = time.Now().UTC()
		// Use CreateIAMPolicy which is a no-op if already exists
		store.CreateIAMPolicy(p)
	}
}

// validateExternalURL checks that a URL does not point to internal/metadata endpoints (SSRF prevention).
func validateExternalURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a host")
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("URL must not point to localhost")
	}
	if strings.HasPrefix(host, "169.254.") || host == "metadata.google.internal" {
		return fmt.Errorf("URL must not point to cloud metadata service")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("URL must not point to loopback, link-local, or private address")
		}
	}
	return nil
}

func (s *Server) Close() {
	if s.rebalancer != nil {
		s.rebalancer.Stop()
	}
	if s.clusterNode != nil {
		s.clusterNode.Shutdown()
	}
	if s.lambdaMgr != nil {
		s.lambdaMgr.Stop()
	}
	if s.rateLimiter != nil {
		s.rateLimiter.Stop()
	}
	if s.accessLog != nil {
		s.accessLog.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}
