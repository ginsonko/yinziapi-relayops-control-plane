package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type staticRetentionConfig struct {
	config intelligence.Config
	err    error
}

func (s staticRetentionConfig) LoadConfig(context.Context) (intelligence.Config, error) {
	return s.config, s.err
}

type scriptedRetentionCleaner struct {
	results  []store.RetentionResult
	policies []store.RetentionPolicy
}

func (s *scriptedRetentionCleaner) CleanupRetention(_ context.Context, _ time.Time,
	policy store.RetentionPolicy,
) (store.RetentionResult, error) {
	s.policies = append(s.policies, policy)
	if len(s.results) == 0 {
		return store.RetentionResult{}, nil
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func TestRetentionPolicyForConfigPreservesConfiguredWindows(t *testing.T) {
	defaults := retentionPolicyForConfig(intelligence.Config{})
	if defaults.OperatingLedger != 8*24*time.Hour {
		t.Fatalf("default ledger retention = %v", defaults.OperatingLedger)
	}
	extended := retentionPolicyForConfig(intelligence.Config{CashLookbackDays: 365, FinancialLookbackDays: 90})
	if extended.OperatingLedger != 366*24*time.Hour {
		t.Fatalf("extended ledger retention = %v", extended.OperatingLedger)
	}
}

func TestRunRetentionCycleStopsAfterEmptyBatch(t *testing.T) {
	cleaner := &scriptedRetentionCleaner{results: []store.RetentionResult{
		{CollectionAttempts: 250}, {Activities: 3}, {}, {OperatingLedger: 99},
	}}
	options := productionRetentionWorkerOptions()
	options.batchPause = 0
	options.maxBatches = 8
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runRetentionCycle(context.Background(), logger, staticRetentionConfig{}, cleaner, options, time.Now())
	if len(cleaner.policies) != 3 {
		t.Fatalf("cleanup calls = %d, want 3", len(cleaner.policies))
	}
	for _, policy := range cleaner.policies {
		if policy.BatchSize != 250 || policy.CollectionKeepLatest != 10 {
			t.Fatalf("unexpected bounded policy = %#v", policy)
		}
	}
}

func TestRunRetentionCycleHonorsMaximumBatches(t *testing.T) {
	cleaner := &scriptedRetentionCleaner{results: make([]store.RetentionResult, 20)}
	for index := range cleaner.results {
		cleaner.results[index].CollectionAttempts = 1
	}
	options := productionRetentionWorkerOptions()
	options.batchPause = 0
	options.maxBatches = 4
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runRetentionCycle(context.Background(), logger, staticRetentionConfig{}, cleaner, options, time.Now())
	if len(cleaner.policies) != 4 {
		t.Fatalf("cleanup calls = %d, want 4", len(cleaner.policies))
	}
}

func TestProductionRetentionUsesFrequentSmallCycles(t *testing.T) {
	options := productionRetentionWorkerOptions()
	if options.interval != 15*time.Minute || options.maxBatches != 1 || options.batchTimeout != 30*time.Second {
		t.Fatalf("production retention options = %#v", options)
	}
}
