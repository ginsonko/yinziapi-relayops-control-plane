package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestServerWriteTimeoutAllowsBoundedInitialCacheBuild(t *testing.T) {
	if serverWriteTimeout != 5*time.Minute {
		t.Fatalf("server write timeout = %s, want 5m", serverWriteTimeout)
	}
	if serverWriteTimeout <= 95*time.Second {
		t.Fatalf("server write timeout = %s, must exceed the former 95s cache-build deadline", serverWriteTimeout)
	}
}

func TestReconcileAfterCollectRunsNativeFirstAndKeepsIndependentFailures(t *testing.T) {
	nativeErr := errors.New("native failed")
	legacyErr := errors.New("legacy failed")
	order := make([]string, 0, 2)
	err := reconcileAfterCollect(context.Background(), 42,
		func(context.Context, int64) error {
			order = append(order, "native")
			return nativeErr
		},
		func(context.Context, int64) error {
			order = append(order, "legacy")
			return legacyErr
		})
	if !reflect.DeepEqual(order, []string{"native", "legacy"}) {
		t.Fatalf("reconciliation order = %v", order)
	}
	if !errors.Is(err, nativeErr) || !errors.Is(err, legacyErr) {
		t.Fatalf("joined error = %v", err)
	}
}
