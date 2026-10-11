package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/kylebegeman/bedrock/internal/compat"
	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/spf13/cobra"
)

// Capabilities deliberately describes the invoked binary, rather than asking a
// running daemon. Upgrade preflight must inspect the candidate build itself.
func newCapabilities(a *app) *cobra.Command {
	return &cobra.Command{
		Use: "capabilities", Short: "Print this build's supported contracts and read-only machine identity.", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := compat.Current(a.secretsStore().KeyPath)
			if err != nil {
				return err
			}
			persisted, err := state.InspectVersion(filepath.Join(a.stateDir, "state.db"))
			if err != nil {
				return err
			}
			info.PersistedStateVersion = persisted
			if a.json {
				return json.NewEncoder(a.stdout).Encode(info)
			}
			_, err = fmt.Fprintf(a.stdout, "%s\nAPI %d; manifest %d; state %d (reads %d..%d)\nMachine: %s\n", info.Build, info.APIVersion, info.ManifestVersion, info.StateVersion, info.MinStateVersion, info.MaxStateVersion, info.MachineID)
			return err
		},
	}
}
