package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddress                  string
	DataDir                        string
	DatabasePath                   string
	MasterKeyPath                  string
	StaticDir                      string
	RequestTimeout                 time.Duration
	SchedulerTick                  time.Duration
	MaxCollectorWorkers            int
	BackgroundWorkersEnabled       bool
	AutomationEnabled              bool
	RemoteWritesEnabled            bool
	AdminToken                     string
	DevelopmentMode                bool
	AllowLoopbackTargets           bool
	BootstrapPath                  string
	RunwayAlertHours               int
	CredentialRefreshInterval      time.Duration
	CredentialRefreshAhead         time.Duration
	CredentialLoginMinInterval     time.Duration
	CredentialLoginInitialBackoff  time.Duration
	CredentialLoginMaximumBackoff  time.Duration
	CredentialLoginMaximumFailures int
	CredentialLoginHumanCooldown   time.Duration
	PairingMaximumAttempts         int
	PairingTransientCooldown       time.Duration
	DNSResolveAttempts             int
	DNSResolveInitialDelay         time.Duration
	DNSResolveMaximumDelay         time.Duration
	DNSResolveJitterPercent        int
	CollectorTransientRetry        time.Duration
	CollectorJoinTimeout           time.Duration
	remoteWritesConfigError        error
}

const minimumAdminTokenLength = 32

func Load() Config {
	dataDir := env("RELAYOPS_DATA_DIR", "data")
	remoteWritesEnabled, remoteWritesConfigError := strictBoolEnv("RELAYOPS_REMOTE_WRITES_ENABLED", false)
	return Config{
		ListenAddress:                  env("RELAYOPS_LISTEN", "127.0.0.1:18180"),
		DataDir:                        dataDir,
		DatabasePath:                   filepath.Join(dataDir, "relayops.db"),
		MasterKeyPath:                  filepath.Join(dataDir, "master.key"),
		StaticDir:                      env("RELAYOPS_STATIC_DIR", filepath.Join("web", "dist")),
		RequestTimeout:                 durationEnv("RELAYOPS_REQUEST_TIMEOUT", 20*time.Second),
		SchedulerTick:                  durationEnv("RELAYOPS_SCHEDULER_TICK", 10*time.Second),
		MaxCollectorWorkers:            intEnv("RELAYOPS_COLLECTOR_WORKERS", 4),
		BackgroundWorkersEnabled:       boolEnv("RELAYOPS_BACKGROUND_WORKERS_ENABLED", false),
		AutomationEnabled:              boolEnv("RELAYOPS_AUTOMATION_ENABLED", false),
		RemoteWritesEnabled:            remoteWritesEnabled,
		AdminToken:                     os.Getenv("RELAYOPS_ADMIN_TOKEN"),
		DevelopmentMode:                boolEnv("RELAYOPS_DEV_MODE", false),
		AllowLoopbackTargets:           boolEnv("RELAYOPS_ALLOW_LOOPBACK_TARGETS", false),
		BootstrapPath:                  os.Getenv("RELAYOPS_BOOTSTRAP_FILE"),
		RunwayAlertHours:               intEnv("RELAYOPS_RUNWAY_ALERT_HOURS", 48),
		CredentialRefreshInterval:      durationEnv("RELAYOPS_CREDENTIAL_REFRESH_INTERVAL", 5*time.Minute),
		CredentialRefreshAhead:         durationEnv("RELAYOPS_CREDENTIAL_REFRESH_AHEAD", 15*time.Minute),
		CredentialLoginMinInterval:     durationEnv("RELAYOPS_CREDENTIAL_LOGIN_MIN_INTERVAL", 15*time.Minute),
		CredentialLoginInitialBackoff:  durationEnv("RELAYOPS_CREDENTIAL_LOGIN_INITIAL_BACKOFF", time.Minute),
		CredentialLoginMaximumBackoff:  durationEnv("RELAYOPS_CREDENTIAL_LOGIN_MAXIMUM_BACKOFF", time.Hour),
		CredentialLoginMaximumFailures: intEnv("RELAYOPS_CREDENTIAL_LOGIN_MAXIMUM_FAILURES", 5),
		CredentialLoginHumanCooldown:   durationEnv("RELAYOPS_CREDENTIAL_LOGIN_HUMAN_COOLDOWN", 12*time.Hour),
		PairingMaximumAttempts:         intEnv("RELAYOPS_PAIRING_MAXIMUM_ATTEMPTS", 3),
		PairingTransientCooldown:       durationEnv("RELAYOPS_PAIRING_TRANSIENT_COOLDOWN", 10*time.Second),
		DNSResolveAttempts:             intEnv("RELAYOPS_DNS_RESOLVE_ATTEMPTS", 3),
		DNSResolveInitialDelay:         durationEnv("RELAYOPS_DNS_RESOLVE_INITIAL_DELAY", 100*time.Millisecond),
		DNSResolveMaximumDelay:         durationEnv("RELAYOPS_DNS_RESOLVE_MAXIMUM_DELAY", 500*time.Millisecond),
		DNSResolveJitterPercent:        nonnegativeIntEnv("RELAYOPS_DNS_RESOLVE_JITTER_PERCENT", 20),
		CollectorTransientRetry:        durationEnv("RELAYOPS_COLLECTOR_TRANSIENT_RETRY", time.Minute),
		CollectorJoinTimeout:           durationEnv("RELAYOPS_COLLECTOR_JOIN_TIMEOUT", 3*time.Minute),
		remoteWritesConfigError:        remoteWritesConfigError,
	}
}

func (c Config) Validate() error {
	if c.remoteWritesConfigError != nil {
		return c.remoteWritesConfigError
	}
	token := strings.TrimSpace(c.AdminToken)
	if token == "" && !c.DevelopmentMode {
		return errors.New("RELAYOPS_ADMIN_TOKEN is required unless RELAYOPS_DEV_MODE=true")
	}
	if token != "" && len(token) < minimumAdminTokenLength {
		return fmt.Errorf("RELAYOPS_ADMIN_TOKEN must contain at least %d characters", minimumAdminTokenLength)
	}
	listenAddress := strings.TrimSpace(c.ListenAddress)
	if listenAddress == "" {
		listenAddress = "127.0.0.1:18180"
	}
	host, _, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return fmt.Errorf("invalid RELAYOPS_LISTEN: %w", err)
	}
	if token == "" && !loopbackHost(host) {
		return errors.New("development mode without an admin token may only listen on loopback")
	}
	if c.AllowLoopbackTargets && !c.DevelopmentMode {
		return errors.New("RELAYOPS_ALLOW_LOOPBACK_TARGETS requires RELAYOPS_DEV_MODE=true")
	}
	if c.AllowLoopbackTargets && !loopbackHost(host) {
		return errors.New("loopback target access requires RelayOps itself to listen on loopback")
	}
	if c.PairingTransientCooldown != 0 && (c.PairingTransientCooldown < time.Second || c.PairingTransientCooldown > time.Minute) {
		return errors.New("RELAYOPS_PAIRING_TRANSIENT_COOLDOWN must be between 1s and 1m")
	}
	if c.DNSResolveAttempts != 0 && (c.DNSResolveAttempts < 1 || c.DNSResolveAttempts > 5) {
		return errors.New("RELAYOPS_DNS_RESOLVE_ATTEMPTS must be between 1 and 5")
	}
	if c.DNSResolveInitialDelay != 0 && (c.DNSResolveInitialDelay < 10*time.Millisecond || c.DNSResolveInitialDelay > 2*time.Second) {
		return errors.New("RELAYOPS_DNS_RESOLVE_INITIAL_DELAY must be between 10ms and 2s")
	}
	if c.DNSResolveMaximumDelay != 0 && ((c.DNSResolveInitialDelay != 0 && c.DNSResolveMaximumDelay < c.DNSResolveInitialDelay) || c.DNSResolveMaximumDelay > 5*time.Second) {
		return errors.New("RELAYOPS_DNS_RESOLVE_MAXIMUM_DELAY must be at least the initial delay and no more than 5s")
	}
	if c.DNSResolveJitterPercent < 0 || c.DNSResolveJitterPercent > 50 {
		return errors.New("RELAYOPS_DNS_RESOLVE_JITTER_PERCENT must be between 0 and 50")
	}
	if c.CollectorTransientRetry != 0 && (c.CollectorTransientRetry < 30*time.Second || c.CollectorTransientRetry > 10*time.Minute) {
		return errors.New("RELAYOPS_COLLECTOR_TRANSIENT_RETRY must be between 30s and 10m")
	}
	if c.CollectorJoinTimeout != 0 && (c.CollectorJoinTimeout < 30*time.Second || c.CollectorJoinTimeout > 5*time.Minute) {
		return errors.New("RELAYOPS_COLLECTOR_JOIN_TIMEOUT must be between 30s and 5m")
	}
	return nil
}

func loopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func intEnv(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func nonnegativeIntEnv(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func boolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func strictBoolEnv(key string, fallback bool) (bool, error) {
	raw, configured := os.LookupEnv(key)
	if !configured {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return false, fmt.Errorf("%s must be a valid boolean, got an empty or whitespace-only value", key)
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a valid boolean: %w", key, err)
	}
	return parsed, nil
}
