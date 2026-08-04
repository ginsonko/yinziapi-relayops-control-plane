package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseStorageBytesIncludesSQLiteSidecars(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "relayops.db")
	for name, content := range map[string]string{path: "main", path + "-wal": "write-ahead", path + "-shm": "shared"} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{path: path}
	got, err := st.DatabaseStorageBytes()
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len("main") + len("write-ahead") + len("shared"))
	if got != want {
		t.Fatalf("DatabaseStorageBytes() = %d, want %d", got, want)
	}
}
