package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/dns"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/host"
)

func newDNS(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "The records behind every host this machine routes, as Cloudflare and the world see them.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			routes, err := apps.Routes(ctx, store)
			if err != nil {
				return err
			}
			if len(routes) == 0 {
				fmt.Fprintln(a.stdout, "no hosts routed yet")
				return nil
			}
			addrs := host.Addresses(ctx, host.RealEnv())
			mgr, cfErr := apps.DNSManager(a.secretsStore(), addrs)
			var statuses []dns.Status
			if cfErr == nil {
				if statuses, err = mgr.Look(ctx, routes); err != nil {
					return err
				}
			} else {
				// Without the integration, what the world resolves.
				for _, r := range routes {
					st := dns.Status{Host: r.Host, App: r.App, Mode: string(r.Mode)}
					if st.Mode == "" {
						st.Mode = "manual"
					}
					ok, pointsAt, err := edge.Resolves(ctx, r.Host, addrs)
					switch {
					case err != nil:
						st.Verdict = err.Error()
					case ok:
						st.Verdict, st.Record = "resolves here", strings.Join(pointsAt, " ")
					default:
						st.Verdict, st.Record = "resolves elsewhere", strings.Join(pointsAt, " ")
					}
					statuses = append(statuses, st)
				}
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(statuses)
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tAPP\tRECORD KEPT BY\tPOINTS AT\tVERDICT")
			for _, st := range statuses {
				kept := "you (manual)"
				switch st.Mode {
				case "direct":
					kept = "quark, direct"
				case "proxied":
					kept = "quark, proxied"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", st.Host, st.App, kept, orDash(st.Record), st.Verdict)
			}
			w.Flush()
			if cfErr != nil {
				fmt.Fprintf(a.stdout, "(what public DNS answers; set the cloudflare integration to see and keep the records themselves)\n")
			}
			return nil
		},
	}
	audit := &cobra.Command{
		Use:   "audit",
		Short: "Records that point here without a route, records quark left behind, routes whose records point elsewhere.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			routes, err := apps.Routes(ctx, store)
			if err != nil {
				return err
			}
			mgr, err := apps.DNSManager(a.secretsStore(), host.Addresses(ctx, host.RealEnv()))
			if err != nil {
				return fmt.Errorf("an audit reads the zones through the cloudflare integration: %w", err)
			}
			findings, err := mgr.Audit(ctx, routes)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(findings)
			}
			if len(findings) == 0 {
				fmt.Fprintln(a.stdout, "every record that points here has a route, and every route has a record")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tRECORD\tFINDING")
			for _, f := range findings {
				record := "-"
				if f.Type != "" {
					record = f.Type + " " + f.Content
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", f.Host, record, f.What)
			}
			return w.Flush()
		},
	}
	var (
		planOnly bool
		proxied  bool
	)
	point := &cobra.Command{
		Use:   "point <host>",
		Short: "Make a host's record name this machine, taking it from wherever it points now (moving day).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.operate(cmd.Context(), apps.PointKind, apps.PointInput{Host: args[0], Proxied: proxied}, planOnly)
		},
	}
	point.Flags().BoolVar(&proxied, "proxied", false, "put the host behind Cloudflare's proxy")
	a.mutatingFlags(point, &planOnly)
	cmd.AddCommand(audit, point)
	return cmd
}
