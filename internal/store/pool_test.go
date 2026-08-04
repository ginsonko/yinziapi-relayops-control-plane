package store

import (
	"path/filepath"
	"testing"
)

func TestOpenUsesBoundedConcurrentSQLitePool(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if maximum := st.db.Stats().MaxOpenConnections; maximum != 4 {
		t.Fatalf("max open SQLite connections = %d, want 4", maximum)
	}
}
