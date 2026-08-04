package targets

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestTargetCatalogCacheCoalescesAndCachesSuccessfulReads(t *testing.T) {
	cache := newTargetCatalogCache()
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) (domain.CatalogSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return domain.CatalogSnapshot{SupplierID: 17}, nil
	}
	type result struct {
		snapshot domain.CatalogSnapshot
		err      error
	}
	results := make(chan result, 2)
	go func() {
		snapshot, err := cache.read(context.Background(), "target", load)
		results <- result{snapshot: snapshot, err: err}
	}()
	<-started
	go func() {
		snapshot, err := cache.read(context.Background(), "target", load)
		results <- result{snapshot: snapshot, err: err}
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || got.snapshot.SupplierID != 17 {
			t.Fatalf("shared result = %#v, %v", got.snapshot, got.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying calls = %d, want 1", calls.Load())
	}
	if _, err := cache.read(context.Background(), "target", load); err != nil {
		t.Fatalf("later read: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("later read did not reuse success; calls = %d", calls.Load())
	}
	if _, err := cache.read(WithFreshRead(context.Background()), "target", load); err != nil {
		t.Fatalf("fresh read: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("fresh read did not bypass cache; calls = %d", calls.Load())
	}
}

func TestTargetCatalogCacheUsesStructuralReadLifetime(t *testing.T) {
	cache := newTargetCatalogCache()
	if cache.successTTL != targetStructuralReadTTL {
		t.Fatalf("catalog success TTL = %s, want %s", cache.successTTL, targetStructuralReadTTL)
	}
	if targetStructuralReadTTL != 4*time.Hour {
		t.Fatalf("structural read TTL = %s, want four hours", targetStructuralReadTTL)
	}
	if cache.failureTTL != targetReadFailureTTL {
		t.Fatalf("catalog failure TTL = %s, want %s", cache.failureTTL, targetReadFailureTTL)
	}
}

func TestTargetCatalogCacheCachesFailuresUntilFreshRetry(t *testing.T) {
	cache := newTargetCatalogCache()
	var calls atomic.Int64
	load := func(context.Context) (domain.CatalogSnapshot, error) {
		if calls.Add(1) == 1 {
			return domain.CatalogSnapshot{}, errors.New("temporary failure")
		}
		return domain.CatalogSnapshot{SupplierID: 23}, nil
	}
	if _, err := cache.read(context.Background(), "target", load); err == nil {
		t.Fatal("first read unexpectedly succeeded")
	}
	if _, err := cache.read(context.Background(), "target", load); err == nil || calls.Load() != 1 {
		t.Fatalf("failure was not shared; err=%v calls=%d", err, calls.Load())
	}
	snapshot, err := cache.read(WithFreshRead(context.Background()), "target", load)
	if err != nil || snapshot.SupplierID != 23 || calls.Load() != 2 {
		t.Fatalf("retry result = %#v, %v; calls = %d", snapshot, err, calls.Load())
	}
}
