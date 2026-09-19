// Command quark is the Quark command line and, on a machine it manages, the
// host daemon. One binary serves both.
package main

import (
	"os"

	"github.com/kylebegeman/quark/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
