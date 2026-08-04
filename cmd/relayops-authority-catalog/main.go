package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type catalogReceipt struct {
	Schema         string   `json:"schema"`
	CatalogSHA256  string   `json:"catalog_sha256"`
	CatalogBytes   int      `json:"catalog_bytes"`
	ActivationUUID string   `json:"activation_uuid,omitempty"`
	Tables         []string `json:"tables"`
	Objects        []string `json:"objects"`
	CreatedAt      string   `json:"created_at"`
}

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("expected produce, compare, or migrate-clone command"))
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "produce":
		err = runProduce(ctx, os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	case "migrate-clone":
		err = runMigrateClone(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func runProduce(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("produce", flag.ContinueOnError)
	dbPath := flags.String("fresh-db", "", "new SQLite database")
	catalogPath := flags.String("catalog", "", "new catalog output")
	receiptPath := flags.String("receipt", "", "new receipt output")
	createNew := flags.Bool("create-new", false, "require all outputs to be absent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*createNew || *dbPath == "" || *catalogPath == "" || *receiptPath == "" {
		return errors.New("produce requires --fresh-db, --catalog, --receipt and --create-new")
	}
	for _, path := range []string{*dbPath, *catalogPath, *receiptPath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("path must be absolute: %s", path)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("output already exists: %s", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		return err
	}
	migration, err := st.ActivateCollectionAuthorityV2(ctx, store.AuthorityMigrationOptions{})
	if err != nil {
		return err
	}
	catalog, err := st.AuditCollectionAuthority(ctx)
	if err != nil {
		return err
	}
	if err := writeExclusiveFile(*catalogPath, catalog.Bytes, 0o600); err != nil {
		return err
	}
	receipt := catalogReceipt{Schema: "r30-authority-catalog-receipt-v1",
		CatalogSHA256: hex.EncodeToString(catalog.AggregateDigest[:]), CatalogBytes: len(catalog.Bytes),
		ActivationUUID: migration.ActivationUUID, Tables: catalog.Tables, Objects: catalog.ObjectNames,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return writeExclusiveJSON(*receiptPath, receipt)
}

func runCompare(args []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	pathA := flags.String("catalog-a", "", "first catalog")
	pathB := flags.String("catalog-b", "", "second catalog")
	receiptPath := flags.String("receipt", "", "new receipt")
	createNew := flags.Bool("create-new", false, "require receipt to be absent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*createNew || *pathA == "" || *pathB == "" || *receiptPath == "" {
		return errors.New("compare requires --catalog-a, --catalog-b, --receipt and --create-new")
	}
	a, err := os.ReadFile(*pathA)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*pathB)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("authority catalog outputs are not byte-identical")
	}
	digest := sha256.Sum256(a)
	return writeExclusiveJSON(*receiptPath, map[string]any{"schema": "r30-authority-catalog-comparison-v1",
		"equal": true, "bytes": len(a), "sha256": hex.EncodeToString(digest[:])})
}

func runMigrateClone(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate-clone", flag.ContinueOnError)
	clone := flags.String("clone", "", "standalone source clone")
	stage := flags.String("stage", "", "new migration stage database")
	restore := flags.String("restore-reentry", "", "new untouched restore database")
	receiptPath := flags.String("receipt", "", "new migration receipt")
	remoteWrites := flags.String("remote-writes", "", "must be disabled")
	targetWrites := flags.String("target-writes", "", "must be disabled")
	createNew := flags.Bool("create-new-outputs", false, "require outputs to be absent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*createNew || *remoteWrites != "disabled" || *targetWrites != "disabled" {
		return errors.New("clone migration requires create-new outputs and disabled remote/target writes")
	}
	for _, path := range []string{*clone, *stage, *restore, *receiptPath} {
		if path == "" || !filepath.IsAbs(path) {
			return errors.New("clone migration paths must be non-empty and absolute")
		}
	}
	if samePath(*clone, *stage) || samePath(*clone, *restore) || samePath(*stage, *restore) {
		return errors.New("clone, stage and restore paths must differ")
	}
	for _, path := range []string{*stage, *restore, *receiptPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("output already exists: %s", path)
		}
	}
	sourceDigest, sourceBytes, err := copyExclusiveWithDigest(*clone, *stage)
	if err != nil {
		return err
	}
	restoreDigest, restoreBytes, err := copyExclusiveWithDigest(*clone, *restore)
	if err != nil {
		return err
	}
	if sourceDigest != restoreDigest || sourceBytes != restoreBytes {
		return errors.New("untouched restore clone differs from migration input")
	}
	st, err := store.Open(*stage)
	if err != nil {
		return err
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		_ = st.Close()
		return err
	}
	migration, err := st.ActivateCollectionAuthorityV2(ctx, store.AuthorityMigrationOptions{})
	if err != nil {
		_ = st.Close()
		return err
	}
	catalog, err := st.AuditCollectionAuthority(ctx)
	closeErr := st.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	stageDigest, stageBytes, err := digestFile(*stage)
	if err != nil {
		return err
	}
	receipt := map[string]any{
		"schema": "r30-authority-clone-migration-v1", "source_sha256": hex.EncodeToString(sourceDigest[:]),
		"source_bytes": sourceBytes, "restore_sha256": hex.EncodeToString(restoreDigest[:]),
		"restore_bytes": restoreBytes, "stage_sha256": hex.EncodeToString(stageDigest[:]), "stage_bytes": stageBytes,
		"activation_uuid": migration.ActivationUUID, "catalog_sha256": hex.EncodeToString(catalog.AggregateDigest[:]),
		"legacy_lease_sha256": hex.EncodeToString(migration.LegacyLeaseDigest[:]),
		"stable_lease_sha256": hex.EncodeToString(migration.StableLeaseDigest[:]),
		"final_lease_sha256":  hex.EncodeToString(migration.FinalLeaseDigest[:]),
		"remote_writes":       "disabled", "target_writes": "disabled",
	}
	return writeExclusiveJSON(*receiptPath, receipt)
}

func writeExclusiveJSON(path string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeExclusiveFile(path, payload, 0o600)
}

func writeExclusiveFile(path string, payload []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		_ = file.Close()
		if owned {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	owned = false
	return nil
}

func copyExclusiveWithDigest(source, destination string) ([sha256.Size]byte, int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	in, err := os.Open(source)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	owned := true
	defer func() {
		_ = out.Close()
		if owned {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, hash), in)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if err := out.Sync(); err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if err := out.Close(); err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	owned = false
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, written, nil
}

func digestFile(path string) ([sha256.Size]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, written, nil
}

func samePath(left, right string) bool {
	left, _ = filepath.Abs(left)
	right, _ = filepath.Abs(right)
	return filepath.Clean(left) == filepath.Clean(right)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
