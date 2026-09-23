package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the code does now")

// Every command's help is its contract with the person reading it: its
// name, arguments, words and flags. The file is every command's, hidden
// ones included.
func TestEveryCommandsHelpStaysWhatItWas(t *testing.T) {
	// Defaults read from the environment would make the file differ from
	// one machine to the next.
	t.Setenv("BEDROCK_STATE_DIR", "~/.bedrock")
	t.Setenv("BEDROCK_SOCKET", "/run/bedrock/bedrock.sock")
	root := newRoot(io.Discard, io.Discard)
	var b strings.Builder
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		var usage bytes.Buffer
		c.SetOut(&usage)
		fmt.Fprintf(&b, "== %s (hidden %v)\n%s\n%s\n", c.CommandPath(), c.Hidden, c.Short, c.Long)
		b.WriteString(c.UsageString())
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub)
		}
	}
	walk(root)
	got := b.String()
	path := filepath.Join("testdata", "help.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("a command's help changed; diff testdata/help.golden against:\n%s", got)
	}
}
