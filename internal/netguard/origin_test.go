package netguard

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSameOriginNormalizesDefaultPorts(t *testing.T) {
	same, err := SameOrigin("https://EXAMPLE.com/path", "https://example.com:443/other")
	if err != nil || !same {
		t.Fatalf("SameOrigin() = %v, %v; want true", same, err)
	}
	same, err = SameOrigin("https://example.com", "http://example.com")
	if err != nil || same {
		t.Fatalf("SameOrigin() = %v, %v; want false", same, err)
	}
}

func TestNormalizeSameOriginPath(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "/keys", want: "/keys", ok: true},
		{input: " /console/token/ ", want: "/console/token/", ok: true},
		{input: "/密钥", want: "/%E5%AF%86%E9%92%A5", ok: true},
		{input: "", want: "", ok: true},
		{input: "https://evil.example/keys"},
		{input: "//evil.example/keys"},
		{input: "/keys?token=secret"},
		{input: "/keys#secret"},
		{input: "/keys\\admin"},
		{input: "/keys%5cadmin"},
		{input: "/keys/../admin"},
		{input: "/keys/%2e%2e/admin"},
		{input: "/keys\nnext"},
		{input: "/keys%0anext"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeSameOriginPath(test.input)
			if test.ok && (err != nil || got != test.want) {
				t.Fatalf("NormalizeSameOriginPath(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
			if !test.ok && err == nil {
				t.Fatalf("NormalizeSameOriginPath(%q) unexpectedly returned %q", test.input, got)
			}
		})
	}
}

func TestValidatePublicIPsRejectsInternalAndMixedAnswers(t *testing.T) {
	for _, input := range [][]net.IP{
		{net.ParseIP("127.0.0.1")},
		{net.ParseIP("10.0.0.1")},
		{net.ParseIP("169.254.169.254")},
		{net.ParseIP("100.64.0.1")},
		{net.ParseIP("2606:4700:4700::1111"), net.ParseIP("::1")},
	} {
		if err := validatePublicIPs("blocked.example", input); err == nil {
			t.Fatalf("validatePublicIPs(%v) unexpectedly succeeded", input)
		}
	}
	if err := validatePublicIPs("public.example", []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}); err != nil {
		t.Fatalf("public addresses rejected: %v", err)
	}
}

func TestValidateOutboundIPsAllowsOnlyLoopbackInDevelopmentMode(t *testing.T) {
	if err := validateOutboundIPs("localhost", []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, true); err != nil {
		t.Fatalf("loopback addresses rejected: %v", err)
	}
	for _, input := range [][]net.IP{
		{net.ParseIP("10.0.0.1")},
		{net.ParseIP("169.254.169.254")},
		{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1")},
	} {
		if err := validateOutboundIPs("blocked.local", input, true); err == nil {
			t.Fatalf("development validation unexpectedly accepted %v", input)
		}
	}
}

func TestSameOriginClientRejectsCrossOriginRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := SameOriginClient(source.Client(), source.URL)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, source.URL, nil)
	request.Header.Set("X-API-Key", "must-not-leave-origin")
	if _, err := client.Do(request); err == nil {
		t.Fatal("expected cross-origin redirect to be rejected")
	}
}

func TestGuardedDialTLSFallsBackBeforeSendingPOSTAndKeepsHTTP2(t *testing.T) {
	t.Parallel()
	const requestBody = "credential-body-must-reach-only-the-healthy-address"
	var healthyRequests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		healthyRequests.Add(1)
		body, _ := io.ReadAll(request.Body)
		if string(body) != requestBody {
			t.Errorf("healthy request body = %q", body)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	serverTLS := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	serverTLS.NextProtos = []string{"h2", "http/1.1"}
	badHandshakeBytes := make(chan []byte, 1)
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("203.0.113.11")}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "203.0.113.10:") {
			client, upstream := net.Pipe()
			go func() {
				defer upstream.Close()
				buffer := make([]byte, 8192)
				count, _ := upstream.Read(buffer)
				badHandshakeBytes <- append([]byte(nil), buffer[:count]...)
			}()
			return client, nil
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	transport := &http.Transport{
		DialTLSContext:    guardedDialTLSContextWithResolver(dial, lookup, serverTLS, false),
		TLSClientConfig:   serverTLS,
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Post("https://example.com/validate", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST after TLS fallback: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 2 {
		t.Fatalf("response = status %d protocol %s", response.StatusCode, response.Proto)
	}
	if healthyRequests.Load() != 1 {
		t.Fatalf("healthy request count = %d", healthyRequests.Load())
	}
	select {
	case captured := <-badHandshakeBytes:
		if bytes.Contains(captured, []byte(requestBody)) {
			t.Fatal("failed TLS candidate received the HTTP request body")
		}
	case <-time.After(time.Second):
		t.Fatal("failed TLS candidate did not receive a handshake")
	}
}

func TestGuardedDialTLSRejectsMixedDNSBeforeDialing(t *testing.T) {
	t.Parallel()
	var dialCount atomic.Int32
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.20"), net.ParseIP("127.0.0.1")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("must not dial")
	}
	_, err := guardedDialTLSContextWithResolver(dial, lookup, &tls.Config{}, false)(context.Background(), "tcp", "mixed.example:443")
	if err == nil || !strings.Contains(err.Error(), "blocked address") {
		t.Fatalf("mixed DNS error = %v", err)
	}
	if dialCount.Load() != 0 {
		t.Fatalf("dial count = %d; want zero", dialCount.Load())
	}
}

func TestGuardedDialTLSHonorsCancellation(t *testing.T) {
	t.Parallel()
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.30")}, nil
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := guardedDialTLSContextWithResolver(dial, lookup, &tls.Config{}, false)(ctx, "tcp", "cancel.example:443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestResolveValidatedIPsRetriesTransientLookupAndRevalidatesAnswer(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		if calls.Add(1) == 1 {
			return nil, &net.DNSError{Err: "temporary resolver failure", Name: "retry.example", IsTemporary: true}
		}
		return []net.IP{net.ParseIP("203.0.113.40")}, nil
	}
	policy := ResolvePolicy{Attempts: 2, InitialDelay: time.Millisecond, MaximumDelay: time.Millisecond}
	ips, err := resolveValidatedIPsWithPolicy(context.Background(), lookup, "retry.example", false, policy)
	if err != nil || len(ips) != 1 || calls.Load() != 2 {
		t.Fatalf("resolved IPs=%v calls=%d err=%v", ips, calls.Load(), err)
	}
}

func TestResolveValidatedIPsDoesNotRetryBlockedOrEmptyAnswers(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string][]net.IP{
		"mixed": {net.ParseIP("203.0.113.41"), net.ParseIP("127.0.0.1")},
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			lookup := func(context.Context, string, string) ([]net.IP, error) {
				calls.Add(1)
				return answer, nil
			}
			policy := ResolvePolicy{Attempts: 5, InitialDelay: time.Millisecond, MaximumDelay: time.Millisecond}
			if _, err := resolveValidatedIPsWithPolicy(context.Background(), lookup, name+".example", false, policy); err == nil {
				t.Fatal("unsafe answer unexpectedly succeeded")
			}
			if calls.Load() != 1 {
				t.Fatalf("unsafe answer was retried %d times", calls.Load())
			}
		})
	}
}

func TestResolveValidatedIPsCancellationStopsRetryBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		calls.Add(1)
		cancel()
		return nil, &net.DNSError{Err: "temporary resolver failure", Name: "cancel.example", IsTemporary: true}
	}
	policy := ResolvePolicy{Attempts: 5, InitialDelay: time.Second, MaximumDelay: time.Second}
	_, err := resolveValidatedIPsWithPolicy(ctx, lookup, "cancel.example", false, policy)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("cancellation error=%v calls=%d", err, calls.Load())
	}
}
