package remotewrite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type countingTransport struct {
	calls atomic.Int64
}

type redirectingTransport struct {
	status           int
	location         string
	calls            atomic.Int64
	destinationCalls atomic.Int64
}

func (t *redirectingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	if request.URL.EscapedPath() == "/api/catalog/query" {
		return &http.Response{StatusCode: t.status, Header: http.Header{"Location": []string{t.location}},
			Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	}
	t.destinationCalls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
}

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func TestDisabledGateBlocksEveryUnmarkedNonSafeMethodBeforeTransport(t *testing.T) {
	underlying := &countingTransport{}
	client := &http.Client{Transport: New(false).Wrap(underlying)}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		request, err := http.NewRequestWithContext(context.Background(), method, "https://target.example/api/write?secret=hidden", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil || !errors.Is(err, ErrDisabled) || !strings.Contains(err.Error(), DisabledMessage) {
			t.Fatalf("%s response=%v error=%v", method, response, err)
		}
		var requestError *url.Error
		if !errors.As(err, &requestError) || requestError.Err == nil || strings.Contains(requestError.Err.Error(), "secret=hidden") {
			t.Fatalf("transport guard leaked its query string: %v", err)
		}
	}
	if calls := underlying.calls.Load(); calls != 0 {
		t.Fatalf("blocked requests reached the underlying transport %d times", calls)
	}
}

func TestDisabledGatePreservesSafeMethodsAndExplicitReadOnlyPost(t *testing.T) {
	underlying := &countingTransport{}
	client := &http.Client{Transport: New(false).Wrap(underlying)}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		request, _ := http.NewRequestWithContext(context.Background(), method, "https://target.example/api/read", nil)
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s response=%v error=%v", method, response, err)
		}
		response.Body.Close()
	}
	readOnlyContext, err := WithReadOnlyRequest(context.Background(), http.MethodPost,
		"https://target.example/api/catalog/query", nil)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(readOnlyContext, http.MethodPost, "https://target.example/api/catalog/query", nil)
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("read-only POST response=%v error=%v", response, err)
	}
	response.Body.Close()
	if calls := underlying.calls.Load(); calls != 4 {
		t.Fatalf("allowed requests reached transport %d times, want 4", calls)
	}
}

func TestSemanticReadPermitDoesNotFollowRedirectsToAnotherRequest(t *testing.T) {
	body := []byte(`{"accounts":[1]}`)
	for _, test := range []struct {
		name     string
		status   int
		location string
	}{
		{name: "same-origin 302 becomes GET", status: http.StatusFound, location: "https://target.example/api/option/"},
		{name: "same-origin 307 preserves POST", status: http.StatusTemporaryRedirect, location: "https://target.example/api/option/"},
		{name: "same-origin 308 preserves POST", status: http.StatusPermanentRedirect, location: "https://target.example/api/channel/"},
		{name: "cross-origin 307", status: http.StatusTemporaryRedirect, location: "https://other.example/api/option/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			underlying := &redirectingTransport{status: test.status, location: test.location}
			client := &http.Client{Transport: New(false).Wrap(underlying)}
			requestContext, err := WithReadOnlyRequest(context.Background(), http.MethodPost,
				"https://target.example/api/catalog/query", body)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(requestContext, http.MethodPost,
				"https://target.example/api/catalog/query", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrDisabled) {
				t.Fatalf("redirect response=%v error=%v, want remote-write gate", response, err)
			}
			if underlying.calls.Load() != 1 || underlying.destinationCalls.Load() != 0 {
				t.Fatalf("redirect reached underlying calls=%d destination=%d", underlying.calls.Load(), underlying.destinationCalls.Load())
			}
		})
	}
}

func TestSemanticReadPermitBindsQueryAndBody(t *testing.T) {
	underlying := &countingTransport{}
	client := &http.Client{Transport: New(false).Wrap(underlying)}
	requestContext, err := WithReadOnlyRequest(context.Background(), http.MethodPost,
		"https://target.example/api/catalog/query?scope=one", []byte(`{"scope":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(requestContext, http.MethodPost,
		"https://target.example/api/catalog/query?scope=two", bytes.NewReader([]byte(`{"scope":"one"}`)))
	if response, err := client.Do(request); response != nil || !errors.Is(err, ErrDisabled) {
		t.Fatalf("query mismatch response=%v error=%v", response, err)
	}
	request, _ = http.NewRequestWithContext(requestContext, http.MethodPost,
		"https://target.example/api/catalog/query?scope=one", bytes.NewReader([]byte(`{"scope":"two"}`)))
	if response, err := client.Do(request); response != nil || !errors.Is(err, ErrDisabled) {
		t.Fatalf("body mismatch response=%v error=%v", response, err)
	}
	if underlying.calls.Load() != 0 {
		t.Fatalf("mismatched permits reached underlying transport %d times", underlying.calls.Load())
	}
}

func TestEnabledAndNilGatesPreserveExistingBehavior(t *testing.T) {
	for _, gate := range []*Gate{New(true), nil} {
		underlying := &countingTransport{}
		client := &http.Client{Transport: gate.Wrap(underlying)}
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, "https://target.example/api/write", nil)
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusOK || underlying.calls.Load() != 1 {
			t.Fatalf("gate=%v response=%v error=%v calls=%d", gate, response, err, underlying.calls.Load())
		}
		response.Body.Close()
	}
}
