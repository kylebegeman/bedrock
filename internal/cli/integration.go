package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kylebegeman/bedrock/internal/cloudflare"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/state"
)

func newIntegration(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "integration",
		Short: "The credentials bedrock itself uses: storage for backups, email for alerts, Cloudflare for DNS.",
	}
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Type an integration's values in, or pipe field=value lines. Values are sealed and never shown.",
		Long:  "Integrations: " + strings.Join(integration.Names(), ", ") + ". In a terminal each field is asked for; otherwise stdin holds one field=value per line.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			def, ok := integration.Lookup(args[0])
			if !ok {
				return fmt.Errorf("no integration named %q; there are %s", args[0], strings.Join(integration.Names(), ", "))
			}
			store := a.secretsStore()
			if err := a.ensureSecretsKey(store); err != nil {
				return err
			}
			current, err := integration.Get(store, def.Name)
			if err != nil && !errors.Is(err, integration.ErrNotSet) {
				return err
			}
			values, err := readIntegrationValues(a, def, current)
			if err != nil {
				return err
			}
			if def.Name == integration.CloudflareName {
				if err := verifyCloudflare(cmd.Context(), a, values, current); err != nil {
					return err
				}
			}
			version, generated, err := integration.Set(store, def.Name, values)
			if err != nil {
				return err
			}
			for field, value := range generated {
				fmt.Fprintf(a.stderr, "\nbedrock made a %s for %s. It is shown once, here, and nowhere else. Keep it with the recovery identity in your password manager; a new machine needs it to read these backups:\n\n  %s\n\n", field, def.Name, value)
			}
			fmt.Fprintf(a.stdout, "%s set (bedrock's secrets version %d)\n", def.Name, version)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the integrations, which are set, and when they were last used.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			statuses, err := integration.List(a.secretsStore())
			if err != nil {
				return err
			}
			uses := map[string]state.IntegrationUse{}
			if store, err := a.openState(); err == nil {
				uses, _ = store.IntegrationUses(cmd.Context())
				store.Close()
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Integrations []integration.Status            `json:"integrations"`
					Uses         map[string]state.IntegrationUse `json:"uses"`
				}{statuses, uses})
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tFOR\tSET\tFIELDS\tLAST USED")
			for _, st := range statuses {
				set, fields, used := "no", "-", "-"
				if st.Set {
					set = st.At.Local().Format("2006-01-02 15:04")
					var fs []string
					for _, k := range sortedKeys(st.Fields) {
						fs = append(fs, k+"="+st.Fields[k])
					}
					fields = strings.Join(fs, " ")
				}
				if u, ok := uses[st.Name]; ok {
					used = fmt.Sprintf("%s (%s)", agoWord(u.UsedAt, time.Now().UTC()), u.Purpose)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", st.Name, st.Purpose, set, fields, used)
			}
			return w.Flush()
		},
	}
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Drop an integration's values.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			version, err := integration.Remove(a.secretsStore(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s removed (bedrock's secrets version %d)\n", args[0], version)
			return nil
		},
	}
	cmd.AddCommand(set, list, remove)
	return cmd
}

// verifyCloudflare tries the token before it is kept: a token that can't
// list its zones would fail the first deploy that needs a record, later
// and less clearly.
func verifyCloudflare(ctx context.Context, a *app, values, current map[string]string) error {
	token, api := values["token"], values["api"]
	if token == "" {
		token = current["token"]
	}
	if _, named := values["api"]; !named {
		api = current["api"]
	}
	client := cloudflare.New(token)
	if api != "" {
		client.Base = api
	}
	zones, err := client.Verify(ctx)
	if err != nil {
		return fmt.Errorf("the cloudflare token doesn't work: %w", err)
	}
	fmt.Fprintf(a.stderr, "the token sees %d zone(s): %s\n", len(zones), strings.Join(zones, ", "))
	return nil
}

// readIntegrationValues asks for each field in a terminal, hiding
// secrets, or reads field=value lines from stdin otherwise. When the
// integration is already set, a blank answer keeps the current value and
// piped lines change only the fields they name.
func readIntegrationValues(a *app, def integration.Definition, current map[string]string) (map[string]string, error) {
	values := map[string]string{}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("expected field=value, got %q", line)
			}
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		return values, sc.Err()
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(a.stderr, "%s: %s\n", def.Name, def.Purpose)
	if len(current) > 0 {
		fmt.Fprintln(a.stderr, "  it is set already; a blank answer keeps the current value")
	}
	for _, f := range def.Fields {
		prompt := f.Prompt
		switch {
		case current[f.Name] != "" && f.Secret:
			prompt += " [kept]"
		case current[f.Name] != "":
			prompt += " [" + current[f.Name] + "]"
		case f.Default != "":
			prompt += " [" + f.Default + "]"
		case f.Optional:
			prompt += " (optional)"
		}
		fmt.Fprintf(a.stderr, "  %s: ", prompt)
		var answer string
		if f.Secret {
			raw, err := term.ReadPassword(fd)
			fmt.Fprintln(a.stderr)
			if err != nil {
				return nil, err
			}
			answer = string(raw)
		} else {
			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				return nil, err
			}
			answer = strings.TrimSpace(line)
		}
		if answer == "" && current[f.Name] != "" {
			continue
		}
		values[f.Name] = answer
	}
	return values, nil
}
