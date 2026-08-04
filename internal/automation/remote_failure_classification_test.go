package automation

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func TestAutomationFailureCategoryPrefersRemoteHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   int
		expected string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, expected: "authorization"},
		{name: "forbidden", status: http.StatusForbidden, expected: "permission_denied"},
		{name: "rate limited", status: http.StatusTooManyRequests, expected: "rate_limited"},
		{name: "internal server error", status: http.StatusInternalServerError, expected: "upstream_5xx"},
		{name: "unavailable", status: http.StatusServiceUnavailable, expected: "upstream_5xx"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := &targets.RemoteError{
				StatusCode: test.status,
				Message:    "authorization and permission diagnostic text must not override the status",
			}
			cause := &policyDiagnosticError{cause: fmt.Errorf("outer permission hint: %w", remote)}
			if actual := automationFailureCategory(cause); actual != test.expected {
				t.Fatalf("category=%q, want %q", actual, test.expected)
			}
		})
	}
}

func TestTargetRateLimitPrefersWrappedRemoteHTTPStatus(t *testing.T) {
	t.Parallel()
	remote := &targets.RemoteError{StatusCode: http.StatusTooManyRequests, Message: "generic remote rejection"}
	if !isTargetRateLimit(fmt.Errorf("target read: %w", remote)) {
		t.Fatal("wrapped structured HTTP 429 was not recognized as a rate limit")
	}
}

func TestAutomationFailureCategoryRetainsTextFallback(t *testing.T) {
	t.Parallel()
	if actual := automationFailureCategory(fmt.Errorf("legacy transport: rate limit")); actual != "rate_limited" {
		t.Fatalf("category=%q, want rate_limited", actual)
	}
}
