package remotewrite

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

const DisabledMessage = "本地验收候选禁止远程写入"

var ErrDisabled = errors.New(DisabledMessage)

type Gate struct {
	enabled atomic.Bool
}

func New(enabled bool) *Gate {
	gate := &Gate{}
	gate.enabled.Store(enabled)
	return gate
}

func (g *Gate) Enabled() bool {
	return g == nil || g.enabled.Load()
}

func (g *Gate) Check(action string) error {
	if g.Enabled() {
		return nil
	}
	action = strings.TrimSpace(action)
	if action == "" {
		return ErrDisabled
	}
	return fmt.Errorf("%w: %s", ErrDisabled, action)
}

func (g *Gate) CheckRequest(ctx context.Context, method, action string) error {
	if safeMethod(method) || hasReadOnlyPermitForMethod(ctx, method) {
		return nil
	}
	return g.Check(action)
}

func (g *Gate) Wrap(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &guardedTransport{gate: g, next: next}
}

type readOnlyRequestKey struct{}

type readOnlyPermit struct {
	method   string
	scheme   string
	host     string
	path     string
	rawQuery string
	bodyHash [sha256.Size]byte
}

// WithReadOnlyRequest grants one exact semantic-read request. Redirects retain
// the context, but a different method, origin, path, query, or body does not
// retain the permit.
func WithReadOnlyRequest(ctx context.Context, method, rawURL string, body []byte) (context.Context, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("semantic-read request URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("semantic-read request URL must use HTTP or HTTPS")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || safeMethod(method) {
		return nil, errors.New("semantic-read permit requires an explicit non-safe HTTP method")
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	permit := readOnlyPermit{method: method, scheme: scheme, host: strings.ToLower(parsed.Host), path: path,
		rawQuery: parsed.RawQuery, bodyHash: sha256.Sum256(body)}
	return context.WithValue(ctx, readOnlyRequestKey{}, permit), nil
}

func readOnlyPermitFromContext(ctx context.Context) (readOnlyPermit, bool) {
	if ctx == nil {
		return readOnlyPermit{}, false
	}
	permit, ok := ctx.Value(readOnlyRequestKey{}).(readOnlyPermit)
	return permit, ok
}

func hasReadOnlyPermitForMethod(ctx context.Context, method string) bool {
	permit, ok := readOnlyPermitFromContext(ctx)
	return ok && permit.method == strings.ToUpper(strings.TrimSpace(method))
}

func requestMatchesReadOnlyPermit(request *http.Request, permit readOnlyPermit) bool {
	if request == nil || request.URL == nil || strings.ToUpper(strings.TrimSpace(request.Method)) != permit.method {
		return false
	}
	path := request.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if strings.ToLower(request.URL.Scheme) != permit.scheme || strings.ToLower(request.URL.Host) != permit.host ||
		path != permit.path || request.URL.RawQuery != permit.rawQuery {
		return false
	}
	bodyHash, ok := requestBodyHash(request)
	return ok && bodyHash == permit.bodyHash
}

func requestBodyHash(request *http.Request) ([sha256.Size]byte, bool) {
	if request.Body == nil || request.Body == http.NoBody {
		return sha256.Sum256(nil), true
	}
	if request.GetBody == nil {
		return [sha256.Size]byte{}, false
	}
	body, err := request.GetBody()
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	defer body.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, body); err != nil {
		return [sha256.Size]byte{}, false
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, true
}

type guardedTransport struct {
	gate *Gate
	next http.RoundTripper
}

func (t *guardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("remote request is nil")
	}
	if !t.gate.Enabled() {
		if permit, ok := readOnlyPermitFromContext(request.Context()); ok {
			if requestMatchesReadOnlyPermit(request, permit) {
				return t.next.RoundTrip(request)
			}
			destination := request.URL.Host + request.URL.EscapedPath()
			return nil, fmt.Errorf("%w: semantic-read permit mismatch for %s %s", ErrDisabled, strings.ToUpper(request.Method), destination)
		}
		if !safeMethod(request.Method) {
			destination := request.URL.Host + request.URL.EscapedPath()
			return nil, fmt.Errorf("%w: blocked %s %s", ErrDisabled, strings.ToUpper(request.Method), destination)
		}
	}
	return t.next.RoundTrip(request)
}

func safeMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
