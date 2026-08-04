package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidateRequiresAdminTokenOutsideDevelopment(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("expected a missing production admin token to be rejected")
	}
	if err := (Config{DevelopmentMode: true, ListenAddress: "127.0.0.1:18180"}).Validate(); err != nil {
		t.Fatalf("explicit development mode was rejected: %v", err)
	}
	if err := (Config{AdminToken: "0123456789abcdef0123456789abcdef", ListenAddress: "0.0.0.0:18180"}).Validate(); err != nil {
		t.Fatalf("configured admin token was rejected: %v", err)
	}
	if err := (Config{AdminToken: strings.Repeat("x", minimumAdminTokenLength-1), ListenAddress: "0.0.0.0:18180"}).Validate(); err == nil {
		t.Fatal("configured 31-character admin token unexpectedly passed validation")
	}
	if err := (Config{DevelopmentMode: true, ListenAddress: "0.0.0.0:18180"}).Validate(); err == nil {
		t.Fatal("tokenless development server unexpectedly accepted a non-loopback listener")
	}
	if err := (Config{AdminToken: "too-short", ListenAddress: "127.0.0.1:18180"}).Validate(); err == nil {
		t.Fatal("short admin token unexpectedly passed validation")
	}
}

func TestCredentialRefreshDurationsAreConfigurable(t *testing.T) {
	t.Setenv("RELAYOPS_CREDENTIAL_REFRESH_INTERVAL", "45s")
	t.Setenv("RELAYOPS_CREDENTIAL_REFRESH_AHEAD", "20m")
	loaded := Load()
	if loaded.CredentialRefreshInterval != 45*time.Second || loaded.CredentialRefreshAhead != 20*time.Minute {
		t.Fatalf("refresh configuration = %v/%v", loaded.CredentialRefreshInterval, loaded.CredentialRefreshAhead)
	}
}

func TestAutomationSafetySwitchIsConfigurable(t *testing.T) {
	t.Setenv("RELAYOPS_AUTOMATION_ENABLED", "")
	if loaded := Load(); loaded.AutomationEnabled {
		t.Fatal("AutomationEnabled defaults to true; public builds must require an explicit opt-in")
	}
	t.Setenv("RELAYOPS_AUTOMATION_ENABLED", "false")
	if loaded := Load(); loaded.AutomationEnabled {
		t.Fatal("AutomationEnabled = true, want false")
	}
	t.Setenv("RELAYOPS_AUTOMATION_ENABLED", "true")
	if loaded := Load(); !loaded.AutomationEnabled {
		t.Fatal("AutomationEnabled = false, want true")
	}
}

func TestBackgroundWorkersDefaultOffAndCanBeEnabled(t *testing.T) {
	t.Setenv("RELAYOPS_BACKGROUND_WORKERS_ENABLED", "")
	if loaded := Load(); loaded.BackgroundWorkersEnabled {
		t.Fatal("background workers must require an explicit opt-in in public builds")
	}
	t.Setenv("RELAYOPS_BACKGROUND_WORKERS_ENABLED", "true")
	if loaded := Load(); !loaded.BackgroundWorkersEnabled {
		t.Fatal("background workers were not explicitly enabled")
	}
}

func TestRemoteWritesDefaultOffAndCanBeEnabled(t *testing.T) {
	t.Setenv("RELAYOPS_REMOTE_WRITES_ENABLED", "temporary-test-value")
	if err := os.Unsetenv("RELAYOPS_REMOTE_WRITES_ENABLED"); err != nil {
		t.Fatal(err)
	}
	if loaded := Load(); loaded.RemoteWritesEnabled {
		t.Fatal("remote writes must require an explicit opt-in in public builds")
	}
	t.Setenv("RELAYOPS_REMOTE_WRITES_ENABLED", "false")
	if loaded := Load(); loaded.RemoteWritesEnabled {
		t.Fatal("remote writes were not disabled for isolated acceptance")
	}
	t.Setenv("RELAYOPS_REMOTE_WRITES_ENABLED", "true")
	if loaded := Load(); !loaded.RemoteWritesEnabled {
		t.Fatal("remote writes were not explicitly enabled")
	}
}

func TestRemoteWritesExplicitInvalidValueFailsClosedAtValidation(t *testing.T) {
	for _, value := range []string{"", "flase", "0x", "   "} {
		t.Run(strings.ReplaceAll(value, " ", "space"), func(t *testing.T) {
			t.Setenv("RELAYOPS_REMOTE_WRITES_ENABLED", value)
			loaded := Load()
			if loaded.RemoteWritesEnabled {
				t.Fatalf("invalid value %q unexpectedly enabled remote writes", value)
			}
			if err := loaded.Validate(); err == nil || !strings.Contains(err.Error(), "RELAYOPS_REMOTE_WRITES_ENABLED") {
				t.Fatalf("invalid value %q validation error=%v", value, err)
			}
		})
	}
}

func TestRemoteWritesStrictBooleanAcceptsCaseAndSurroundingWhitespace(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: " FALSE ", want: false},
		{value: "True", want: true},
	} {
		t.Run(strings.TrimSpace(test.value), func(t *testing.T) {
			t.Setenv("RELAYOPS_REMOTE_WRITES_ENABLED", test.value)
			loaded := Load()
			if loaded.RemoteWritesEnabled != test.want {
				t.Fatalf("value %q loaded=%t want=%t", test.value, loaded.RemoteWritesEnabled, test.want)
			}
			loaded.AdminToken = "0123456789abcdef0123456789abcdef"
			loaded.ListenAddress = "127.0.0.1:18180"
			if err := loaded.Validate(); err != nil {
				t.Fatalf("valid value %q rejected: %v", test.value, err)
			}
		})
	}
}

func TestCredentialLoginPolicyIsConfigurable(t *testing.T) {
	t.Setenv("RELAYOPS_CREDENTIAL_LOGIN_MIN_INTERVAL", "45s")
	t.Setenv("RELAYOPS_CREDENTIAL_LOGIN_INITIAL_BACKOFF", "2m")
	t.Setenv("RELAYOPS_CREDENTIAL_LOGIN_MAXIMUM_BACKOFF", "3h")
	t.Setenv("RELAYOPS_CREDENTIAL_LOGIN_MAXIMUM_FAILURES", "7")
	t.Setenv("RELAYOPS_CREDENTIAL_LOGIN_HUMAN_COOLDOWN", "18h")
	loaded := Load()
	if loaded.CredentialLoginMinInterval != 45*time.Second || loaded.CredentialLoginInitialBackoff != 2*time.Minute ||
		loaded.CredentialLoginMaximumBackoff != 3*time.Hour || loaded.CredentialLoginMaximumFailures != 7 ||
		loaded.CredentialLoginHumanCooldown != 18*time.Hour {
		t.Fatalf("credential login policy = %#v", loaded)
	}
}

func TestLoopbackTargetsAreRestrictedToLoopbackDevelopment(t *testing.T) {
	t.Setenv("RELAYOPS_ALLOW_LOOPBACK_TARGETS", "true")
	loaded := Load()
	if !loaded.AllowLoopbackTargets {
		t.Fatal("loopback target flag was not loaded")
	}
	if err := (Config{AdminToken: "0123456789abcdef0123456789abcdef", ListenAddress: "127.0.0.1:18180", AllowLoopbackTargets: true}).Validate(); err == nil {
		t.Fatal("production mode unexpectedly allowed loopback targets")
	}
	if err := (Config{DevelopmentMode: true, ListenAddress: "0.0.0.0:18180", AllowLoopbackTargets: true}).Validate(); err == nil {
		t.Fatal("non-loopback listener unexpectedly allowed loopback targets")
	}
	if err := (Config{DevelopmentMode: true, ListenAddress: "127.0.0.1:18180", AllowLoopbackTargets: true}).Validate(); err != nil {
		t.Fatalf("loopback development target was rejected: %v", err)
	}
}

func TestNetworkRecoveryPolicyIsConfigurable(t *testing.T) {
	t.Setenv("RELAYOPS_DNS_RESOLVE_ATTEMPTS", "4")
	t.Setenv("RELAYOPS_DNS_RESOLVE_INITIAL_DELAY", "75ms")
	t.Setenv("RELAYOPS_DNS_RESOLVE_MAXIMUM_DELAY", "900ms")
	t.Setenv("RELAYOPS_DNS_RESOLVE_JITTER_PERCENT", "0")
	t.Setenv("RELAYOPS_COLLECTOR_TRANSIENT_RETRY", "75s")
	t.Setenv("RELAYOPS_COLLECTOR_JOIN_TIMEOUT", "2m")
	loaded := Load()
	if loaded.DNSResolveAttempts != 4 || loaded.DNSResolveInitialDelay != 75*time.Millisecond ||
		loaded.DNSResolveMaximumDelay != 900*time.Millisecond || loaded.DNSResolveJitterPercent != 0 ||
		loaded.CollectorTransientRetry != 75*time.Second || loaded.CollectorJoinTimeout != 2*time.Minute {
		t.Fatalf("network recovery policy = %#v", loaded)
	}
	loaded.AdminToken = "0123456789abcdef0123456789abcdef"
	loaded.ListenAddress = "127.0.0.1:18180"
	if err := loaded.Validate(); err != nil {
		t.Fatalf("valid network recovery policy rejected: %v", err)
	}
}
