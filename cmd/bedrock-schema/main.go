// Command bedrock-schema regenerates the manifest editor schema. It is a
// development tool, not an additional deployed program.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

func main() {
	out := flag.String("out", "schema/bedrock.schema.json", "generated schema path")
	flag.Parse()
	body, err := manifest.JSONSchema()
	if err == nil {
		err = os.WriteFile(*out, body, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
