package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	retentionSafetyMarginDays = 1
	maximumForecastDays       = intelligence.MaximumForecastHorizonHours / 24
)

type retentionConfigLoader interface {
	LoadConfig(context.Context) (intelligence.Config, error)
}

type retentionCleaner interface {
	CleanupRetention(context.Context, time.Time, store.RetentionPolicy) (store.RetentionResult, error)
}

type retentionWorkerOptions struct {
	initialDelay time.Duration
	interval     time.Duration
	batchPause   time.Duration
	batchTimeout time.Duration
	maxBatches   int
}

func productionRetentionWorkerOptions() retentionWorkerOptions {
	return retentionWorkerOptions{
		initialDelay: 2 * time.Minute,
		interval:     15 * time.Minute,
		batchPause:   250 * time.Millisecond,
		batchTimeout: 30 * time.Second,
		maxBatches:   1,
	}
}

func runRetentionWorker(ctx context.Context, logger *slog.Logger, configs retentionConfigLoader,
	cleaner retentionCleaner, options retentionWorkerOptions,
) {
	if options.initialDelay < 0 || options.interval <= 0 || options.batchPause < 0 ||
		options.batchTimeout <= 0 || options.maxBatches <= 0 {
		logger.Error("invalid retention worker configuration")
		return
	}
	timer := time.NewTimer(options.initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runRetentionCycle(ctx, logger, configs, cleaner, options, time.Now().UTC())
			timer.Reset(options.interval)
		}
	}
}

func runRetentionCycle(ctx context.Context, logger *slog.Logger, configs retentionConfigLoader,
	cleaner retentionCleaner, options retentionWorkerOptions, now time.Time,
) {
	started := time.Now()
	config, err := configs.LoadConfig(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Warn("load retention window", "error", err)
		}
		return
	}
	policy := retentionPolicyForConfig(config)
	total := store.RetentionResult{}
	batches := 0
	for batches < options.maxBatches {
		batchCtx, cancel := context.WithTimeout(ctx, options.batchTimeout)
		result, cleanupErr := cleaner.CleanupRetention(batchCtx, now, policy)
		cancel()
		if cleanupErr != nil {
			if !errors.Is(cleanupErr, context.Canceled) {
				logger.Warn("bounded retention cleanup", "error", cleanupErr, "completed_batches", batches,
					"duration_ms", time.Since(started).Milliseconds())
			}
			return
		}
		batches++
		addRetentionResult(&total, result)
		if retentionResultEmpty(result) {
			break
		}
		if batches < options.maxBatches && !waitForRetentionPause(ctx, options.batchPause) {
			return
		}
	}
	logger.Info("bounded retention cleanup completed", "batches", batches,
		"operating_ledger", total.OperatingLedger, "catalog_snapshots", total.CatalogSnapshots,
		"price_observations", total.PriceObservations, "balance_observations", total.BalanceObservations,
		"activities", total.Activities, "pairing_tokens", total.PairingTokens,
		"collection_attempts", total.CollectionAttempts, "duration_ms", time.Since(started).Milliseconds())
}

func retentionPolicyForConfig(config intelligence.Config) store.RetentionPolicy {
	config = intelligence.NormalizeConfig(config)
	lookbackDays := maximumForecastDays
	if config.CashLookbackDays > lookbackDays {
		lookbackDays = config.CashLookbackDays
	}
	if config.FinancialLookbackDays > lookbackDays {
		lookbackDays = config.FinancialLookbackDays
	}
	policy := store.DefaultRetentionPolicy()
	required := time.Duration(lookbackDays+retentionSafetyMarginDays) * 24 * time.Hour
	if policy.OperatingLedger < required {
		policy.OperatingLedger = required
	}
	return policy
}

func retentionResultEmpty(result store.RetentionResult) bool {
	return result.OperatingLedger == 0 && result.CatalogSnapshots == 0 && result.PriceObservations == 0 &&
		result.BalanceObservations == 0 && result.Activities == 0 && result.PairingTokens == 0 &&
		result.CollectionAttempts == 0
}

func addRetentionResult(total *store.RetentionResult, result store.RetentionResult) {
	total.OperatingLedger += result.OperatingLedger
	total.CatalogSnapshots += result.CatalogSnapshots
	total.PriceObservations += result.PriceObservations
	total.BalanceObservations += result.BalanceObservations
	total.Activities += result.Activities
	total.PairingTokens += result.PairingTokens
	total.CollectionAttempts += result.CollectionAttempts
}

func waitForRetentionPause(ctx context.Context, pause time.Duration) bool {
	if pause == 0 {
		return true
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
