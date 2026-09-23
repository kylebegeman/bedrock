package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
)

// secretsStore is the machine's store.
func (a *app) secretsStore() *secrets.Store { return secrets.DefaultStore(a.stateDir) }

func newSecret(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Keep an app's secrets sealed on this machine.",
	}
	cmd.AddCommand(newSecretSet(a), newSecretList(a), newSecretRemove(a), newSecretVersions(a), newSecretCopy(a), newSecretRecipient(a), newSecretExport(a), newSecretImport(a))
	return cmd
}

// newSecretSet is bedrock secret set.
func newSecretSet(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <app> <NAME>",
		Short: "Store a value, read from stdin or typed without echo. Never pass it as an argument.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			appName, name := args[0], args[1]
			if !secrets.ValidName(name) {
				return fmt.Errorf("%q must be an UPPER_CASE name", name)
			}
			store := a.secretsStore()
			if err := a.ensureSecretsKey(store); err != nil {
				return err
			}
			value, err := readSecretValue(a)
			if err != nil {
				return err
			}
			version, err := store.Set(appName, name, value)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s set; secrets version %d. The next deploy uses it.\n", appName, name, version)
			return nil
		},
	}
	return cmd
}

// newSecretList is bedrock secret list.
func newSecretList(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List the names in the current version. Values are never shown.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			names, version, err := a.secretsStore().Names(args[0])
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Version int      `json:"version"`
					Names   []string `json:"names"`
				}{version, names})
			}
			if version == 0 {
				fmt.Fprintf(a.stdout, "%s has no secrets yet\n", args[0])
				return nil
			}
			fmt.Fprintf(a.stdout, "%s, secrets version %d:\n", args[0], version)
			for _, n := range names {
				fmt.Fprintf(a.stdout, "  %s\n", n)
			}
			return nil
		},
	}
	return cmd
}

// newSecretRemove is bedrock secret remove.
func newSecretRemove(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <app> <NAME>",
		Short: "Drop a name from the next version.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			version, err := a.secretsStore().Remove(args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s removed; secrets version %d\n", args[0], args[1], version)
			return nil
		},
	}
	return cmd
}

// newSecretVersions is bedrock secret versions.
func newSecretVersions(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "versions <app>",
		Short: "List every version and the names it holds.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			vs, err := a.secretsStore().Versions(args[0])
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(vs)
			}
			if len(vs) == 0 {
				fmt.Fprintf(a.stdout, "%s has no secrets yet\n", args[0])
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "VERSION\tWHEN\tNAMES")
			for _, v := range vs {
				fmt.Fprintf(w, "%d\t%s\t%s\n", v.Version, v.At.Local().Format("2006-01-02 15:04"), strings.Join(v.Names, ", "))
			}
			return w.Flush()
		},
	}
	return cmd
}

// newSecretCopy is bedrock secret copy.
func newSecretCopy(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "copy <from-app> <NAME> <to-app>",
		Short: "Give another app the same value of a secret, without showing it.",
		Long: `Give another app the same value of a secret, without showing it: for
two apps that must share a credential, such as a worker that pairs with
the product that made its token. The value never leaves the machine's
store. Copying the same value again changes nothing.`,
		Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			from, name, to := args[0], args[1], args[2]
			if from == manifest.ReservedApp || to == manifest.ReservedApp {
				return fmt.Errorf("the integrations' credentials stay bedrock's own")
			}
			if from == to {
				return fmt.Errorf("%s would copy onto itself", name)
			}
			store := a.secretsStore()
			values, _, err := store.LoadCurrent(from)
			if err != nil {
				return err
			}
			value, ok := values[name]
			if !ok {
				return fmt.Errorf("%s has no secret named %s", from, name)
			}
			current, _, err := store.LoadCurrent(to)
			if err != nil {
				return err
			}
			if existing, ok := current[name]; ok && existing == value {
				fmt.Fprintf(a.stdout, "%s: %s already matches %s's\n", to, name, from)
				return nil
			}
			version, err := store.Set(to, name, value)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s copied from %s; secrets version %d. The next deploy uses it.\n", to, name, from, version)
			return nil
		},
	}
	return cmd
}

// newSecretRecipient is bedrock secret recipient.
func newSecretRecipient(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use: "recipient", Short: "Print the machine's public encryption recipient, creating its key if needed.", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Automated provisioning must not print the recovery identity. It
			// remains in the root-only key file for deliberate recovery setup.
			public, _, _, err := a.secretsStore().EnsureKey()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.stdout, public)
			return err
		},
	}
	return cmd
}

// newSecretExport is bedrock secret export.
func newSecretExport(a *app) *cobra.Command {
	var recipientKey string
	cmd := &cobra.Command{
		Use: "export <from-app> <NAME> <to-app>", Short: "Seal one secret for another machine; prints only age ciphertext.", Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			value, err := a.secretsStore().Export(args[0], args[1], args[2], recipientKey)
			if err != nil {
				return err
			}
			_, err = io.WriteString(a.stdout, value)
			return err
		},
	}
	cmd.Flags().StringVar(&recipientKey, "recipient", "", "the receiving machine's public age recipient")
	_ = cmd.MarkFlagRequired("recipient")
	return cmd
}

// newSecretImport is bedrock secret import.
func newSecretImport(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use: "import <app> <NAME>", Short: "Read a secret sealed for this machine from stdin, without showing it.", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			version, err := a.secretsStore().Import(args[0], args[1], os.Stdin)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s received; secrets version %d\n", args[0], args[1], version)
			return nil
		},
	}
	return cmd
}

// ensureSecretsKey creates the machine's key on first use and shows the
// recovery identity exactly once.
func (a *app) ensureSecretsKey(store *secrets.Store) error {
	_, created, identity, err := store.EnsureKey()
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(a.stderr, "This machine now has a secrets key at %s.\nIts recovery identity is shown once, here, and nowhere else. Keep it in your password manager:\n\n  %s\n\n", store.KeyPath, identity)
	}
	return nil
}

// readSecretValue takes the value from stdin: a line typed without echo
// in a terminal, or everything piped in otherwise.
func readSecretValue(a *app) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(a.stderr, "Value (not shown): ")
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", err
		}
		if len(raw) == 0 {
			return "", errors.New("nothing entered")
		}
		return string(raw), nil
	}
	raw, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return "", err
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", errors.New("nothing on stdin; pipe the value in, or run this in a terminal to type it")
	}
	return value, nil
}
