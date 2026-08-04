package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type receipt struct {
	Batches              int   `json:"batches"`
	OperatingLedger      int64 `json:"operating_ledger"`
	CatalogSnapshots     int64 `json:"catalog_snapshots"`
	PriceObservations    int64 `json:"price_observations"`
	BalanceObservations  int64 `json:"balance_observations"`
	Activities           int64 `json:"activities"`
	PairingTokens        int64 `json:"pairing_tokens"`
	CollectionAttempts   int64 `json:"collection_attempts"`
	DurationMilliseconds int64 `json:"duration_ms"`
	Converged            bool  `json:"converged"`
}

func main() {
	databasePath := flag.String("database", "", "stopped RelayOps SQLite database copy")
	maxBatches := flag.Int("max-batches", 10000, "maximum bounded cleanup transactions")
	pause := flag.Duration("pause", 250*time.Millisecond, "pause between cleanup transactions")
	batchTimeout := flag.Duration("batch-timeout", 30*time.Second, "deadline for one cleanup transaction")
	nowRaw := flag.String("now", "", "optional RFC3339 retention clock")
	flag.Parse()
	if *databasePath == "" || *maxBatches <= 0 || *pause < 0 || *batchTimeout <= 0 {
		log.Fatal("database, positive max-batches, non-negative pause, and positive batch-timeout are required")
	}
	now := time.Now().UTC()
	if *nowRaw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *nowRaw)
		if err != nil {
			log.Fatalf("parse now: %v", err)
		}
		now = parsed.UTC()
	}

	total, runErr := runRetention(*databasePath, *maxBatches, *pause, *batchTimeout, now)
	encoded, err := json.Marshal(total)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(encoded))
	if runErr != nil {
		log.Fatal(runErr)
	}
}

func runRetention(databasePath string, maxBatches int, pause, batchTimeout time.Duration, now time.Time) (receipt, error) {
	st, err := store.Open(databasePath)
	if err != nil {
		return receipt{}, err
	}
	defer st.Close()
	policy := store.DefaultRetentionPolicy()
	started := time.Now()
	total := receipt{}
	for total.Batches < maxBatches {
		ctx, cancel := context.WithTimeout(context.Background(), batchTimeout)
		result, cleanupErr := st.CleanupRetention(ctx, now, policy)
		cancel()
		if cleanupErr != nil {
			return total, fmt.Errorf("cleanup batch %d: %w", total.Batches+1, cleanupErr)
		}
		total.Batches++
		add(&total, result)
		if empty(result) {
			total.Converged = true
			break
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	total.DurationMilliseconds = time.Since(started).Milliseconds()
	if !total.Converged {
		return total, fmt.Errorf("retention did not converge within %d batches", maxBatches)
	}
	return total, nil
}

func add(total *receipt, result store.RetentionResult) {
	total.OperatingLedger += result.OperatingLedger
	total.CatalogSnapshots += result.CatalogSnapshots
	total.PriceObservations += result.PriceObservations
	total.BalanceObservations += result.BalanceObservations
	total.Activities += result.Activities
	total.PairingTokens += result.PairingTokens
	total.CollectionAttempts += result.CollectionAttempts
}

func empty(result store.RetentionResult) bool {
	return result.OperatingLedger == 0 && result.CatalogSnapshots == 0 && result.PriceObservations == 0 &&
		result.BalanceObservations == 0 && result.Activities == 0 && result.PairingTokens == 0 &&
		result.CollectionAttempts == 0
}
