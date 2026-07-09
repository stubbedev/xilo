package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/config"
)

// schemaCmd generates the JSON Schema for xilo.yaml from the config.Config
// struct + its doc comments. CI regenerates and publishes schemas/xilo.schema.json.
func schemaCmd() *cobra.Command {
	c := &cobra.Command{Use: "schema", Short: "JSON schema tools"}
	var out string
	dump := &cobra.Command{
		Use:   "dump",
		Short: "Write the config JSON schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := &jsonschema.Reflector{
				FieldNameTag:   "yaml",
				ExpandedStruct: true,
				DoNotReference: true,
			}
			// Pull field descriptions from the Go doc comments. Best-effort:
			// works when run from the repo root (CI, `just sync-schema`).
			_ = r.AddGoComments("github.com/stubbedev/xilo", "./internal/config")
			b, err := json.MarshalIndent(r.Reflect(&config.Config{}), "", "  ")
			if err != nil {
				return err
			}
			b = append(b, '\n')
			if out == "" {
				fmt.Print(string(b))
				return nil
			}
			return os.WriteFile(out, b, 0o644)
		},
	}
	dump.Flags().StringVar(&out, "out", "", "write to file instead of stdout")
	c.AddCommand(dump)
	return c
}
