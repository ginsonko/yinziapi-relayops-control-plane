package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	maxRedirects          = 5
	ControlPlaneUserAgent = "RelayOps-Collector/1.0"
)

// NormalizeSameOriginPath accepts only a path that can be resolved against an
// already validated origin without changing authority or carrying secrets in
// a query string. It is intended for administrator-facing navigation links,
// not arbitrary outbound request targets.
func NormalizeSameOriginPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 512 || strings.Contains(raw, "\\") {
		return "", errors.New("path must be a bounded same-origin absolute path")
	}
	for _, character := range raw {
		if character < 0x20 || character == 0x7f {
			return "", errors.New("path contains a control character")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path == "" ||
		!strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "", errors.New("path must be a same-origin absolute path without query or fragment")
	}
	if strings.Contains(parsed.Path, "\\") {
		return "", errors.New("path cannot contain a backslash")
	}
	for _, character := range parsed.Path {
		if character < 0x20 || character == 0x7f {
			return "", errors.New("path contains an encoded control character")
		}
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("path cannot contain dot segments")
		}
	}
	cleaned := path.Clean(parsed.Path)
	if cleaned != parsed.Path && cleaned+"/" != parsed.Path {
		return "", errors.New("path is not canonical")
	}
	if strings.HasSuffix(parsed.Path, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return (&url.URL{Path: cleaned}).EscapedPath(), nil
}

// ApplyControlPlaneUserAgent gives login, refresh, validation, and collection
// requests one stable client identity. Some upstreams bind issued access tokens
// to the login request's User-Agent, so changing it mid-session invalidates an
// otherwise valid credential.
func ApplyControlPlaneUserAgent(request *http.Request) {
	if request != nil && strings.TrimSpace(request.Header.Get("User-Agent")) == "" {
		request.Header.Set("User-Agent", ControlPlaneUserAgent)
	}
}

type Origin struct {
	Scheme string
	Host   string
	Port   string
}

// ResolvePolicy bounds retries that happen before any TCP connection is
// opened. It never retries an address set that failed the outbound-address
// safety checks.
type ResolvePolicy struct {
	Attempts      int
	InitialDelay  time.Duration
	MaximumDelay  time.Duration
	JitterPercent int
}

func DefaultResolvePolicy() ResolvePolicy {
	return ResolvePolicy{Attempts: 3, InitialDelay: 100 * time.Millisecond, MaximumDelay: 500 * time.Millisecond, JitterPercent: 20}
}

func normalizeResolvePolicy(policy ResolvePolicy) ResolvePolicy {
	defaults := DefaultResolvePolicy()
	if policy.Attempts < 1 {
		policy.Attempts = defaults.Attempts
	}
	if policy.Attempts > 5 {
		policy.Attempts = 5
	}
	if policy.InitialDelay <= 0 {
		policy.InitialDelay = defaults.InitialDelay
	}
	if policy.MaximumDelay < policy.InitialDelay {
		policy.MaximumDelay = policy.InitialDelay
	}
	if policy.MaximumDelay > 5*time.Second {
		policy.MaximumDelay = 5 * time.Second
	}
	if policy.JitterPercent < 0 {
		policy.JitterPercent = 0
	}
	if policy.JitterPercent > 50 {
		policy.JitterPercent = 50
	}
	return policy
}

func ParseOrigin(raw string) (Origin, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return Origin{}, fmt.Errorf("invalid HTTP origin %q", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return Origin{}, fmt.Errorf("unsupported origin scheme %q", parsed.Scheme)
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return Origin{
		Scheme: scheme,
		Host:   strings.TrimSuffix(strings.ToLower(parsed.Hostname()), "."),
		Port:   port,
	}, nil
}

func SameOrigin(left, right string) (bool, error) {
	leftOrigin, err := ParseOrigin(left)
	if err != nil {
		return false, err
	}
	rightOrigin, err := ParseOrigin(right)
	if err != nil {
		return false, err
	}
	return leftOrigin == rightOrigin, nil
}

func RequireSameOrigin(credentialSite, destination string) error {
	same, err := SameOrigin(credentialSite, destination)
	if err != nil {
		return err
	}
	if !same {
		return errors.New("credential origin does not match the destination origin")
	}
	return nil
}

// PublicDialContext resolves every connection attempt itself, rejects the
// complete answer set when it contains a non-public address, and dials the
// validated IP directly. Repeating this check for every new connection closes
// the DNS-rebinding gap between URL validation and transport dialing.
func PublicDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return PublicDialContextWithPolicy(dialer, DefaultResolvePolicy())
}

func PublicDialContextWithPolicy(dialer *net.Dialer, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialContextWithPolicy(dialer, false, policy)
}

// PublicOrLoopbackDialContext exists only for an explicitly enabled local
// development environment. It still blocks private, link-local, shared,
// multicast, unspecified, and mixed DNS answers.
func PublicOrLoopbackDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return PublicOrLoopbackDialContextWithPolicy(dialer, DefaultResolvePolicy())
}

func PublicOrLoopbackDialContextWithPolicy(dialer *net.Dialer, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialContextWithPolicy(dialer, true, policy)
}

// PublicDialTLSContext resolves and validates the complete DNS answer before
// trying any address. An address is selected only after its TLS handshake and
// hostname verification succeed, so a broken CDN edge cannot mask healthy
// addresses returned for the same host.
func PublicDialTLSContext(dialer *net.Dialer, tlsConfig *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	return PublicDialTLSContextWithPolicy(dialer, tlsConfig, DefaultResolvePolicy())
}

func PublicDialTLSContextWithPolicy(dialer *net.Dialer, tlsConfig *tls.Config, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialTLSContextWithPolicy(dialer, tlsConfig, false, policy)
}

// PublicOrLoopbackDialTLSContext is the development-only counterpart of
// PublicDialTLSContext. It permits loopback answers but preserves all other
// address, SNI, and certificate checks.
func PublicOrLoopbackDialTLSContext(dialer *net.Dialer, tlsConfig *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	return PublicOrLoopbackDialTLSContextWithPolicy(dialer, tlsConfig, DefaultResolvePolicy())
}

func PublicOrLoopbackDialTLSContextWithPolicy(dialer *net.Dialer, tlsConfig *tls.Config, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialTLSContextWithPolicy(dialer, tlsConfig, true, policy)
}

func guardedDialContext(dialer *net.Dialer, allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialContextWithPolicy(dialer, allowLoopback, DefaultResolvePolicy())
}

func guardedDialContextWithPolicy(dialer *net.Dialer, allowLoopback bool, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return guardedDialContextWithResolverPolicy(dialer.DialContext, net.DefaultResolver.LookupIP, allowLoopback, policy)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)
type lookupIPFunc func(context.Context, string, string) ([]net.IP, error)

func guardedDialContextWithResolver(dial dialContextFunc, lookup lookupIPFunc, allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialContextWithResolverPolicy(dial, lookup, allowLoopback, DefaultResolvePolicy())
}

func guardedDialContextWithResolverPolicy(dial dialContextFunc, lookup lookupIPFunc, allowLoopback bool,
	policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid outbound address %q: %w", address, err)
		}
		ips, err := resolveValidatedIPsWithPolicy(ctx, lookup, host, allowLoopback, policy)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if !networkAllowsIP(network, ip) {
				continue
			}
			connection, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("DNS returned no address for the requested network")
		}
		return nil, fmt.Errorf("dial validated outbound host %q: %w", host, lastErr)
	}
}

func guardedDialTLSContext(dialer *net.Dialer, tlsConfig *tls.Config, allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialTLSContextWithPolicy(dialer, tlsConfig, allowLoopback, DefaultResolvePolicy())
}

func guardedDialTLSContextWithPolicy(dialer *net.Dialer, tlsConfig *tls.Config, allowLoopback bool,
	policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return guardedDialTLSContextWithResolverPolicy(dialer.DialContext, net.DefaultResolver.LookupIP, tlsConfig, allowLoopback, policy)
}

func guardedDialTLSContextWithResolver(dial dialContextFunc, lookup lookupIPFunc, baseTLSConfig *tls.Config,
	allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return guardedDialTLSContextWithResolverPolicy(dial, lookup, baseTLSConfig, allowLoopback, DefaultResolvePolicy())
}

func guardedDialTLSContextWithResolverPolicy(dial dialContextFunc, lookup lookupIPFunc, baseTLSConfig *tls.Config,
	allowLoopback bool, policy ResolvePolicy) func(context.Context, string, string) (net.Conn, error) {
	if baseTLSConfig == nil {
		baseTLSConfig = &tls.Config{}
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid outbound TLS address %q: %w", address, err)
		}
		ips, err := resolveValidatedIPsWithPolicy(ctx, lookup, host, allowLoopback, policy)
		if err != nil {
			return nil, err
		}
		var lastErr error
		attempted := 0
		for _, ip := range ips {
			if !networkAllowsIP(network, ip) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			attempted++
			rawConnection, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr != nil {
				lastErr = dialErr
				continue
			}
			candidateConfig := baseTLSConfig.Clone()
			candidateConfig.ServerName = strings.TrimSuffix(host, ".")
			if len(candidateConfig.NextProtos) == 0 {
				candidateConfig.NextProtos = []string{"h2", "http/1.1"}
			}
			tlsConnection := tls.Client(rawConnection, candidateConfig)
			if handshakeErr := tlsConnection.HandshakeContext(ctx); handshakeErr == nil {
				return tlsConnection, nil
			} else {
				lastErr = handshakeErr
				_ = rawConnection.Close()
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if lastErr == nil {
			lastErr = errors.New("DNS returned no address for the requested network")
		}
		return nil, fmt.Errorf("establish TLS to validated outbound host %q after %d address attempt(s): %w", host, attempted, lastErr)
	}
}

func resolveValidatedIPs(ctx context.Context, lookup lookupIPFunc, host string, allowLoopback bool) ([]net.IP, error) {
	return resolveValidatedIPsWithPolicy(ctx, lookup, host, allowLoopback, DefaultResolvePolicy())
}

func resolveValidatedIPsWithPolicy(ctx context.Context, lookup lookupIPFunc, host string, allowLoopback bool,
	policy ResolvePolicy) ([]net.IP, error) {
	policy = normalizeResolvePolicy(policy)
	var lastErr error
	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, "ip", host)
		if err == nil {
			// A syntactically successful lookup is never retried past a safety
			// rejection or empty answer. Each accepted attempt stands alone.
			if validateErr := validateOutboundIPs(host, ips, allowLoopback); validateErr != nil {
				return nil, validateErr
			}
			return ips, nil
		}
		lastErr = err
		if attempt == policy.Attempts {
			break
		}
		if err := waitResolveRetry(ctx, resolveRetryDelay(policy, host, attempt)); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("resolve outbound host %q after %d attempt(s): %w", host, policy.Attempts, lastErr)
}

func waitResolveRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resolveRetryDelay(policy ResolvePolicy, host string, failedAttempt int) time.Duration {
	delay := policy.InitialDelay
	for step := 1; step < failedAttempt; step++ {
		if delay >= policy.MaximumDelay/2 {
			delay = policy.MaximumDelay
			break
		}
		delay *= 2
	}
	if delay > policy.MaximumDelay {
		delay = policy.MaximumDelay
	}
	span := int64(delay) * int64(policy.JitterPercent) / 100
	if span == 0 {
		return delay
	}
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "%s:%d", strings.ToLower(strings.TrimSpace(host)), failedAttempt)
	offset := int64(hash.Sum32())%(2*span+1) - span
	return time.Duration(int64(delay) + offset)
}

func networkAllowsIP(network string, ip net.IP) bool {
	if network == "tcp4" && ip.To4() == nil {
		return false
	}
	if network == "tcp6" && ip.To4() != nil {
		return false
	}
	return true
}

func validatePublicIPs(host string, ips []net.IP) error {
	return validateOutboundIPs(host, ips, false)
}

func validateOutboundIPs(host string, ips []net.IP, allowLoopback bool) error {
	if len(ips) == 0 {
		return fmt.Errorf("outbound host %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) && !(allowLoopback && ip != nil && ip.IsLoopback()) {
			return fmt.Errorf("outbound host %q resolved to blocked address %s", host, ip.String())
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	ip = ip.To16()
	if ip == nil {
		return false
	}
	// RFC 6598 shared address space is not covered by net.IP.IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40 {
		return false
	}
	return true
}

// SameOriginClient clones source and rejects redirects before credentials can
// leave the configured upstream origin.
func SameOriginClient(source *http.Client, origin string) (*http.Client, error) {
	if source == nil {
		source = http.DefaultClient
	}
	expected, err := ParseOrigin(origin)
	if err != nil {
		return nil, err
	}
	clone := *source
	upstreamRedirect := source.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		actual, parseErr := ParseOrigin(req.URL.String())
		if parseErr != nil {
			return parseErr
		}
		if actual != expected {
			return errors.New("redirect escaped the configured origin")
		}
		if upstreamRedirect != nil {
			return upstreamRedirect(req, via)
		}
		return nil
	}
	return &clone, nil
}
