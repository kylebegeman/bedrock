package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
)

func newGC(a *app) *cobra.Command {
	var (
		planOnly  bool
		in        apps.GCInput
		keepCache string
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove containers, images and build cache that no kept revision uses.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keepCache != "" {
				n, err := apps.ParseSize(keepCache)
				if err != nil {
					return fmt.Errorf("--keep-cache: %w", err)
				}
				in.BuildCacheMax = n
			}
			return a.operate(cmd.Context(), apps.GCKind, in, planOnly)
		},
	}
	cmd.Flags().BoolVar(&in.KeepBuildCache, "keep-build-cache", false, "leave Docker's build cache alone")
	cmd.Flags().StringVar(&keepCache, "keep-cache", "", "how much build cache to keep, such as 4g or 512m (default 8g)")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}
