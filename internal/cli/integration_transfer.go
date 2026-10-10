package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/spf13/cobra"
)

func addIntegrationTransfer(cmd *cobra.Command, a *app) {
	var recipient string
	export := &cobra.Command{Use: "export <integration...>", Short: "Seal selected integrations, including the backup password, for a target recipient.", Args: cobra.MinimumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		sealed, err := integration.Export(a.secretsStore(), args, recipient)
		if err != nil {
			return err
		}
		_, err = io.WriteString(a.stdout, sealed)
		return err
	}}
	export.Flags().StringVar(&recipient, "to", "", "target public age recipient")
	_ = export.MarkFlagRequired("to")
	var replace bool
	accept := &cobra.Command{Use: "import <integration...>", Short: "Read a recipient-bound encrypted integration transfer from stdin.", Args: cobra.MinimumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		names, err := integration.Import(a.secretsStore(), args, os.Stdin, replace)
		if err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(a.stdout).Encode(struct {
				Stored []string `json:"stored"`
			}{names})
		}
		fmt.Fprintf(a.stdout, "Imported %d integration(s). Existing matching values were retained.\n", len(names))
		return nil
	}}
	accept.Flags().BoolVar(&replace, "replace", false, "explicitly replace conflicting target integration fields")
	cmd.AddCommand(export, accept)
}
