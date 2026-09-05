// Command schemagen writes schemas/xilo.schema.json from the config.Config
// struct and its doc comments.
//
// It lives outside cmd/xilo on purpose: reflecting a schema pulls in
// invopop/jsonschema and, through it, go/parser and a second YAML library.
// Those are worth ~1 MB of binary that every user would carry for a command
// only `just sync-schema` and CI ever run.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"

	"github.com/stubbedev/xilo/internal/config"
)

func main() {
	out := flag.String("out", "", "write to file instead of stdout")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	r := &jsonschema.Reflector{
		FieldNameTag:   "yaml",
		ExpandedStruct: true,
		DoNotReference: true,
	}
	// Pull field descriptions from the Go doc comments. Best-effort: works
	// when run from the repo root (CI, `just sync-schema`).
	_ = r.AddGoComments("github.com/stubbedev/xilo", "./internal/config")
	b, err := json.MarshalIndent(r.Reflect(&config.Config{}), "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if out == "" {
		_, err := os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(out, b, 0o644)
}
