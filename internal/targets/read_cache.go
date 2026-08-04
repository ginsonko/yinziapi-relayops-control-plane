package targets

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	targetSharedReadTTL     = 30 * time.Minute
	targetStructuralReadTTL = 4 * time.Hour
	targetReadFailureTTL    = 10 * time.Minute
)

type freshTargetReadContextKey struct{}

// WithFreshRead bypasses a shared target cache for the final read immediately
// before a mutation or for an explicit administrator refresh.
func WithFreshRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshTargetReadContextKey{}, true)
}

func freshTargetRead(ctx context.Context) bool {
	value, _ := ctx.Value(freshTargetReadContextKey{}).(bool)
	return value
}

type sharedReadEntry[T any] struct {
	value     T
	err       error
	expiresAt time.Time
}

type sharedReadFlight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

type sharedReadCache[T any] struct {
	mu         sync.Mutex
	successTTL time.Duration
	failureTTL time.Duration
	entries    map[string]sharedReadEntry[T]
	inflight   map[string]*sharedReadFlight[T]
	generation map[string]uint64
}

func newSharedReadCache[T any](successTTL, failureTTL time.Duration) *sharedReadCache[T] {
	return &sharedReadCache[T]{successTTL: successTTL, failureTTL: failureTTL,
		entries: make(map[string]sharedReadEntry[T]), inflight: make(map[string]*sharedReadFlight[T]),
		generation: make(map[string]uint64)}
}

func (c *sharedReadCache[T]) read(ctx context.Context, key string, load func(context.Context) (T, error)) (T, error) {
	var zero T
	if c == nil {
		return load(ctx)
	}
	key = strings.TrimSpace(key)
	if freshTargetRead(ctx) {
		c.invalidate(key)
	}
	now := time.Now()
	c.mu.Lock()
	generation := c.generation[key]
	if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.value, entry.err
	}
	flightKey := key + "\x00" + strconv.FormatUint(generation, 10)
	if flight := c.inflight[flightKey]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-flight.done:
			return flight.value, flight.err
		}
	}
	flight := &sharedReadFlight[T]{done: make(chan struct{})}
	c.inflight[flightKey] = flight
	c.mu.Unlock()

	flight.value, flight.err = load(ctx)
	c.mu.Lock()
	delete(c.inflight, flightKey)
	if c.generation[key] == generation {
		ttl := c.successTTL
		if flight.err != nil {
			ttl = c.failureTTL
		}
		if ttl > 0 {
			c.entries[key] = sharedReadEntry[T]{value: flight.value, err: flight.err, expiresAt: time.Now().Add(ttl)}
		}
	}
	close(flight.done)
	c.mu.Unlock()
	return flight.value, flight.err
}

func (c *sharedReadCache[T]) invalidate(key string) {
	if c == nil {
		return
	}
	key = strings.TrimSpace(key)
	c.mu.Lock()
	c.generation[key]++
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *sharedReadCache[T]) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for key := range c.generation {
		if strings.HasPrefix(key, prefix) {
			c.generation[key]++
			delete(c.entries, key)
		}
	}
	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			c.generation[key]++
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}
