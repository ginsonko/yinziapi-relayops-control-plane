package httpapi

import (
	"testing"
	"time"
)

func TestRequestContextTimeoutAllowsBoundedColdDashboard(t *testing.T) {
	if requestContextTimeout != 4*time.Minute+30*time.Second {
		t.Fatalf("request context timeout = %s, want 4m30s", requestContextTimeout)
	}
	if requestContextTimeout <= 90*time.Second {
		t.Fatalf("request context timeout = %s, must exceed the former 90s cold-dashboard deadline", requestContextTimeout)
	}
}
