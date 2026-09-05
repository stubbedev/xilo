package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The schema is a published artifact (editors fetch it from master), so keep
// one check that reflection still produces a populated object.
func TestRunWritesSchema(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema.json")
	if err := run(out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	props, ok := v["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		t.Fatalf("schema has no properties: %v", v)
	}
	if _, ok := props["data_dir"]; !ok {
		t.Fatalf("schema is missing config fields: %v", props)
	}
}
