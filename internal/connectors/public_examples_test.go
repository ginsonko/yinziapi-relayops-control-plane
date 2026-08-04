package connectors

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicConnectorExamplesMatchCurrentSchemas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		file  string
		parse func([]byte) error
	}{
		{name: "manifest", file: "newapi-fork.manifest.json", parse: func(data []byte) error {
			_, _, err := ParseConnectorManifest(data)
			return err
		}},
		{name: "json connector", file: "catalog.connector.json", parse: func(data []byte) error {
			_, err := ParseJSONConnectorSpec(data)
			return err
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("..", "..", "examples", test.file))
			if err != nil {
				t.Fatal(err)
			}
			if err := test.parse(data); err != nil {
				t.Fatalf("public example %s: %v", test.file, err)
			}
		})
	}
}
