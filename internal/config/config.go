// Package config reads every REDLITE_* value once. .env.example is the authoritative list; nothing
// outside this package calls os.Getenv (the go-check hook enforces it). A .env file in the working
// directory is loaded first, then the real environment overrides it.
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
	LeaderAddr  string
	LogLevel    string

	CacheTTL             time.Duration
	CacheJitterPercent   int
	CacheJanitorInterval time.Duration
	CacheMaxKeysPerSheet int // 0 = unbounded

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

	AdminKey string
}

// IsReplica reports whether this instance follows a leader.
func (c Config) IsReplica() bool { return c.Role == "replica" }

// Load reads .env from dir (missing file is fine), lets the environment override it, and returns the
// typed config. A value that fails to parse is an error naming the variable.
func Load(dir string) (Config, error) {
	if err := loadDotEnv(dir + "/.env"); err != nil {
		return Config{}, err
	}
	var errs []string
	c := Config{
		Port:                   str("REDLITE_PORT", ":8081"),
		Environment:            str("REDLITE_ENVIRONMENT", "development"),
		Role:                   str("REDLITE_ROLE", "leader"),
		LeaderAddr:             str("REDLITE_LEADER_ADDRESS", ""),
		LogLevel:               str("REDLITE_LOG_LEVEL", "info"),
		CacheTTL:               dur("REDLITE_CACHE_TTL", 15*time.Minute, &errs),
		CacheJitterPercent:     num("REDLITE_CACHE_JITTER_PERCENT", 20, &errs),
		CacheJanitorInterval:   dur("REDLITE_CACHE_JANITOR_INTERVAL", time.Minute, &errs),
		CacheMaxKeysPerSheet:   num("REDLITE_CACHE_MAX_KEYS_PER_SHEET", 0, &errs),
		LockTTL:                dur("REDLITE_LOCK_TTL", 30*time.Second, &errs),
		LockCleanupInterval:    dur("REDLITE_LOCK_CLEANUP_INTERVAL", 5*time.Second, &errs),
		HeartbeatInterval:      dur("REDLITE_HEARTBEAT_INTERVAL", 5*time.Second, &errs),
		HeartbeatTimeout:       dur("REDLITE_HEARTBEAT_TIMEOUT", 10*time.Second, &errs),
		PersistEnabled:         boolean("REDLITE_PERSIST_ENABLED", false, &errs),
		PersistDatabaseURL:     str("REDLITE_PERSIST_DATABASE_URL", ""),
		PersistBatchSize:       num("REDLITE_PERSIST_BATCH_SIZE", 100, &errs),
		PersistFlushInterval:   dur("REDLITE_PERSIST_FLUSH_INTERVAL", 5*time.Second, &errs),
		PersistCleanupInterval: dur("REDLITE_PERSIST_CLEANUP_INTERVAL", time.Minute, &errs),
		PersistPoolMax:         num("REDLITE_PERSIST_POOL_MAX", 5, &errs),
		PersistPoolMin:         num("REDLITE_PERSIST_POOL_MIN", 1, &errs),
		AdminKey:               str("REDLITE_ADMIN_KEY", ""),
	}
	if c.Role != "leader" && c.Role != "replica" {
		errs = append(errs, "REDLITE_ROLE must be leader or replica")
	}
	if c.IsReplica() && c.LeaderAddr == "" {
		errs = append(errs, "REDLITE_LEADER_ADDRESS is required when REDLITE_ROLE=replica")
	}
	if c.PersistEnabled && c.PersistDatabaseURL == "" {
		errs = append(errs, "REDLITE_PERSIST_DATABASE_URL is required when REDLITE_PERSIST_ENABLED=true")
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

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

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func num(key string, def int, errs *[]string) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, key+" must be an integer")
		return def
	}
	return n
}

func dur(key string, def time.Duration, errs *[]string) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, key+" must be a duration like 15m or 30s")
		return def
	}
	return d
}

func boolean(key string, def bool, errs *[]string) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, key+" must be true or false")
		return def
	}
	return b
}
