package historysearch

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestCanonicalSchemaPin(t *testing.T) {
	schema, err := os.ReadFile("../../api/agent-search-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if actual := hex.EncodeToString(digest[:]); actual != SchemaSHA256 {
		t.Fatalf("compiled search schema pin %s, canonical %s", SchemaSHA256, actual)
	}
}
