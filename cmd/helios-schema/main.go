// Command helios-schema regenerates the manifest editor schema. It is a
// development tool, not an additional deployed program.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kylebegeman/helios/internal/manifest"
)

func main() {
	out := flag.String("out", "schema/helios.schema.json", "generated schema path")
	legacy := flag.String("legacy-out", "", "optional compatibility schema path")
	flag.Parse()
	body, err := manifest.JSONSchema()
	if err == nil {
		err = os.WriteFile(*out, body, 0o644)
	}
	if err == nil && *legacy != "" {
		body, err = manifest.LegacyJSONSchema()
		if err == nil {
			err = os.WriteFile(*legacy, body, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
