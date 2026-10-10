package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The editor artifact must follow authoritative type changes rather than
// silently continuing to flag a newly supported field as unknown.
func TestPublishedSchemaMatchesItsGenerator(t *testing.T) {
	generated, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(filepath.Join("..", "..", "schema", "bedrock.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, published) {
		t.Fatal("the published editor schema is stale; run go generate ./internal/manifest")
	}
	var schema map[string]any
	if err := json.Unmarshal(published, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["x-bedrock-schema-version"] != SchemaVersion {
		t.Fatalf("the editor schema lacks its declared contract: %v", schema)
	}
}
