// Command bedrock is the compatibility entrypoint for the Helios CLI and daemon.
package main

import (
	"os"

	"github.com/kylebegeman/helios/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
