// Package config reads every REDLITE_* value once. .env.example is the authoritative list; nothing
// outside this package calls os.Getenv (the go-check hook enforces it). A .env file in the working
// directory is loaded first, then the real environment overrides it. The Config value is passed
// down explicitly; there is no package-level config.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the typed view of the environment.
type Config struct {
	Port        string
	Environment string
	Role        string
	LeaderAddr  string // replica: the leader's /replica URL
	LogLevel    string

	// PeerAddr is the other node's base URL (ws://host:port). A leader probes the peer's /health
	// with it and steps down to replica of a peer with a higher epoch (D-012). Empty = no probe.
	PeerAddr        string
	InstanceID      string        // empty = generated at boot
	ShutdownTimeout time.Duration // how long shutdown waits for connections and the persist flush

	CacheTTL             time.Duration // the default for a set with ttl 0/absent; 0 = no expiry (D-003)
	CacheJitterPercent   int
	CacheJanitorInterval time.Duration
	CacheMaxKeysPerSheet int // 0 = unbounded

	SheetAutocreate bool          // D-013: true = the first open creates the sheet (v1 behaviour)
	BcryptCost      int           // sheet password hashing cost; tests lower it
	SweepLogWindow  time.Duration // R6: how long an invalidation is remembered to refuse a stale set
	SweepLogMax     int           // R6: at most this many invalidations remembered per sheet

	ConnSendQueue        int           // D-014: frames queued per client connection before it is closed
	ReplicaSendQueue     int           // frames queued per replica connection before it is closed
	MaxFrameBytes        int64         // read limit on a client frame
	ReplicaMaxFrameBytes int64         // read limit on a replica stream frame (a snapshot_sheet can be large)
	PingInterval         time.Duration // keepalive ping on every connection
	WriteTimeout         time.Duration // deadline for one frame write or ping round trip

	LockTTL             time.Duration
	LockCleanupInterval time.Duration

	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration

	PersistEnabled         bool
	PersistDatabaseURL     string
	PersistBatchSize       int
	PersistFlushInterval   time.Duration
	PersistCleanupInterval time.Duration
	PersistPoolMax         int
	PersistPoolMin         int
	PersistQueueSize       int  // bounded write-behind channel; a full channel drops the op with a Warn
	PersistLogged          bool // D-020: true = cache_entries is made LOGGED at boot (survives a DB crash/failover)
	ControlListen          bool // D-021: LISTEN redlite_control (default true; only when persistence is on)

	// Election is D-022: "epoch" (D-012 only) or "advisory" (pg_try_advisory_lock on a dedicated
	// connection decides the leader; the epoch still fences). advisory needs persistence.
	Election string

	AdminKey string
}

// IsReplica reports whether this instance follows a leader.
func (c Config) IsReplica() bool { return c.Role == "replica" }

// Default returns the config every variable unset would give: what Load returns with an empty
// environment and no .env. The integration harness starts from it and overrides fields.
func Default() Config {
	c, _ := build(func(string) (string, bool) { return "", false })
	return c
}

// Load reads .env from dir (missing file is fine), lets the environment override it, and returns the
// typed config. A value that fails to parse is an error naming the variable.
func Load(dir string) (Config, error) {
	if err := loadDotEnv(dir + "/.env"); err != nil {
		return Config{}, err
	}
	return build(os.LookupEnv)
}

// build reads every variable through lookup and validates the result.
func build(lookup func(string) (string, bool)) (Config, error) {
	e := &env{lookup: lookup}
	c := Config{
		Port:                   e.str("REDLITE_PORT", ":8095"),
		Environment:            e.str("REDLITE_ENVIRONMENT", "development"),
		Role:                   e.str("REDLITE_ROLE", "leader"),
		LeaderAddr:             e.str("REDLITE_LEADER_ADDRESS", ""),
		LogLevel:               e.str("REDLITE_LOG_LEVEL", "info"),
		PeerAddr:               e.str("REDLITE_PEER_ADDRESS", ""),
		InstanceID:             e.str("REDLITE_INSTANCE_ID", ""),
		ShutdownTimeout:        e.dur("REDLITE_SHUTDOWN_TIMEOUT", 10*time.Second),
		CacheTTL:               e.dur("REDLITE_CACHE_TTL", 15*time.Minute),
		CacheJitterPercent:     e.num("REDLITE_CACHE_JITTER_PERCENT", 20),
		CacheJanitorInterval:   e.dur("REDLITE_CACHE_JANITOR_INTERVAL", time.Minute),
		CacheMaxKeysPerSheet:   e.num("REDLITE_CACHE_MAX_KEYS_PER_SHEET", 0),
		SheetAutocreate:        e.boolean("REDLITE_SHEET_AUTOCREATE", true),
		BcryptCost:             e.num("REDLITE_BCRYPT_COST", 10),
		SweepLogWindow:         e.dur("REDLITE_SWEEP_LOG_WINDOW", 30*time.Second),
		SweepLogMax:            e.num("REDLITE_SWEEP_LOG_MAX", 4096),
		ConnSendQueue:          e.num("REDLITE_CONN_SEND_QUEUE", 256),
		ReplicaSendQueue:       e.num("REDLITE_REPLICA_SEND_QUEUE", 4096),
		MaxFrameBytes:          int64(e.num("REDLITE_MAX_FRAME_BYTES", 1<<20)),
		ReplicaMaxFrameBytes:   int64(e.num("REDLITE_REPLICA_MAX_FRAME_BYTES", 64<<20)),
		PingInterval:           e.dur("REDLITE_PING_INTERVAL", 15*time.Second),
		WriteTimeout:           e.dur("REDLITE_WRITE_TIMEOUT", 10*time.Second),
		LockTTL:                e.dur("REDLITE_LOCK_TTL", 30*time.Second),
		LockCleanupInterval:    e.dur("REDLITE_LOCK_CLEANUP_INTERVAL", 5*time.Second),
		HeartbeatInterval:      e.dur("REDLITE_HEARTBEAT_INTERVAL", 5*time.Second),
		HeartbeatTimeout:       e.dur("REDLITE_HEARTBEAT_TIMEOUT", 10*time.Second),
		PersistEnabled:         e.boolean("REDLITE_PERSIST_ENABLED", false),
		PersistDatabaseURL:     e.str("REDLITE_PERSIST_DATABASE_URL", ""),
		PersistBatchSize:       e.num("REDLITE_PERSIST_BATCH_SIZE", 100),
		PersistFlushInterval:   e.dur("REDLITE_PERSIST_FLUSH_INTERVAL", 5*time.Second),
		PersistCleanupInterval: e.dur("REDLITE_PERSIST_CLEANUP_INTERVAL", time.Minute),
		PersistPoolMax:         e.num("REDLITE_PERSIST_POOL_MAX", 5),
		PersistPoolMin:         e.num("REDLITE_PERSIST_POOL_MIN", 1),
		PersistQueueSize:       e.num("REDLITE_PERSIST_QUEUE_SIZE", 10000),
		PersistLogged:          e.boolean("REDLITE_PERSIST_LOGGED", false),
		ControlListen:          e.boolean("REDLITE_CONTROL_LISTEN", true),
		Election:               e.str("REDLITE_ELECTION", "epoch"),
		AdminKey:               e.str("REDLITE_ADMIN_KEY", ""),
	}
	errs := e.errs
	if c.Role != "leader" && c.Role != "replica" {
		errs = append(errs, "REDLITE_ROLE must be leader or replica")
	}
	if c.IsReplica() && c.LeaderAddr == "" {
		errs = append(errs, "REDLITE_LEADER_ADDRESS is required when REDLITE_ROLE=replica")
	}
	if c.PersistEnabled && c.PersistDatabaseURL == "" {
		errs = append(errs, "REDLITE_PERSIST_DATABASE_URL is required when REDLITE_PERSIST_ENABLED=true")
	}
	if c.ConnSendQueue < 1 || c.ReplicaSendQueue < 1 || c.PersistQueueSize < 1 {
		errs = append(errs, "REDLITE_CONN_SEND_QUEUE, REDLITE_REPLICA_SEND_QUEUE and REDLITE_PERSIST_QUEUE_SIZE must be at least 1")
	}
	if c.Election != "epoch" && c.Election != "advisory" {
		errs = append(errs, "REDLITE_ELECTION must be epoch or advisory")
	}
	if c.Election == "advisory" && !c.PersistEnabled {
		errs = append(errs, "REDLITE_ELECTION=advisory requires REDLITE_PERSIST_ENABLED=true")
	}
	if c.HeartbeatTimeout <= c.HeartbeatInterval {
		errs = append(errs, "REDLITE_HEARTBEAT_TIMEOUT must be longer than REDLITE_HEARTBEAT_INTERVAL")
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// TestDatabaseURL returns REDLITE_TEST_DATABASE_URL. Only tests call it, to decide whether persist
// tests run or skip; it is never part of Config and never read in production code.
func TestDatabaseURL() string { return os.Getenv("REDLITE_TEST_DATABASE_URL") }

// loadDotEnv sets KEY=VALUE lines from path into the environment without overriding values already
// set. Blank lines and # comments are skipped; surrounding quotes on a value are stripped.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		v = strings.Trim(v, `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// env reads typed values through lookup and collects one error per variable that fails to parse.
type env struct {
	lookup func(string) (string, bool)
	errs   []string
}

func (e *env) raw(key string) (string, bool) {
	v, ok := e.lookup(key)
	return v, ok && v != ""
}

func (e *env) str(key, def string) string {
	if v, ok := e.raw(key); ok {
		return v
	}
	return def
}

func (e *env) num(key string, def int) int {
	v, ok := e.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, key+" must be an integer")
		return def
	}
	return n
}

func (e *env) dur(key string, def time.Duration) time.Duration {
	v, ok := e.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, key+" must be a duration like 15m or 30s")
		return def
	}
	return d
}

func (e *env) boolean(key string, def bool) bool {
	v, ok := e.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, key+" must be true or false")
		return def
	}
	return b
}
