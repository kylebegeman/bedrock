package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

// Direct secret settings share writer authority with recovery and data
// operations, even though they do not create kernel operation receipts.
func (a *app) guardSecretMutation(cmd *cobra.Command, target func([]string) string) {
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := target(args)
		if name == "" {
			return run(cmd, args)
		}
		if name != manifest.ReservedApp && !manifest.ValidApp(name) {
			return fmt.Errorf("%q is not a valid app name", name)
		}
		stateFile := filepath.Join(a.stateDir, "state.db")
		unlock, err := apps.AcquireDataAccess(cmd.Context(), stateFile, name)
		if err != nil {
			return err
		}
		defer unlock()
		if name == manifest.ReservedApp {
			// Integrations are shared by all apps, including the storage needed
			// to resume an interrupted checkpoint restore.
			if err := apps.CheckReleaseDataAccess(cmd.Context(), stateFile, ""); err != nil {
				return err
			}
		}
		return run(cmd, args)
	}
}
