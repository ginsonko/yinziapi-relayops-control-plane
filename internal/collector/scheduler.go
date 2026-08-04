package collector

import (
	"context"
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type Scheduler struct {
	store   *store.Store
	service *Service
	tick    time.Duration
	workers int

	jobs chan int64
	wake chan struct{}
	mu   sync.Mutex
	busy map[int64]struct{}
}

func NewScheduler(store *store.Store, service *Service, tick time.Duration, workers int) *Scheduler {
	if tick <= 0 {
		tick = 10 * time.Second
	}
	if workers <= 0 {
		workers = 4
	}
	service.SetMaxConcurrent(workers)
	queueSize := workers * 4
	if queueSize < 16 {
		queueSize = 16
	}
	return &Scheduler{
		store: store, service: service, tick: tick, workers: workers,
		jobs: make(chan int64, queueSize), wake: make(chan struct{}, 1), busy: make(map[int64]struct{}),
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(s.workers)
	for worker := 0; worker < s.workers; worker++ {
		go func() {
			defer workers.Done()
			s.runWorker(ctx)
		}()
	}
	defer workers.Wait()
	s.runDue(ctx)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.runDue(ctx)
		case <-ticker.C:
			s.runDue(ctx)
		}
	}
}

// Wake coalesces scheduler notifications. Collection due state remains in the
// database, so a full wake channel cannot lose work.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// runDue discovers and enqueues work without waiting for a whole collection
// batch. A slow upstream therefore occupies only one worker and cannot keep an
// idle worker from serving later due suppliers.
func (s *Scheduler) runDue(ctx context.Context) {
	scanLimit := cap(s.jobs) + s.workers
	if scanLimit < 64 {
		scanLimit = 64
	}
	suppliers, err := s.store.DueSuppliers(ctx, time.Now().UTC(), scanLimit)
	if err != nil {
		return
	}
	for _, supplier := range suppliers {
		if !s.enqueue(ctx, supplier.ID) {
			if ctx.Err() != nil {
				return
			}
			// A full bounded queue leaves remaining suppliers durably due.
			if len(s.jobs) == cap(s.jobs) {
				return
			}
		}
	}
}

func (s *Scheduler) enqueue(ctx context.Context, supplierID int64) bool {
	s.mu.Lock()
	if _, exists := s.busy[supplierID]; exists {
		s.mu.Unlock()
		return false
	}
	s.busy[supplierID] = struct{}{}
	select {
	case s.jobs <- supplierID:
		s.mu.Unlock()
		return true
	case <-ctx.Done():
		delete(s.busy, supplierID)
		s.mu.Unlock()
		return false
	default:
		delete(s.busy, supplierID)
		s.mu.Unlock()
		return false
	}
}

func (s *Scheduler) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case supplierID := <-s.jobs:
			_, _ = s.service.collect(ctx, supplierID, true, false)
			s.mu.Lock()
			delete(s.busy, supplierID)
			s.mu.Unlock()
			s.Wake()
		}
	}
}
