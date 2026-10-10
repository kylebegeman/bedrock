package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/remote"
	"github.com/kylebegeman/bedrock/internal/secrets"
)

// SSH deliberately has no remote terminal. Read typed credentials at the
// client's terminal and send them only through SSH's standard input.
func (a *app) targetCredentialInput(ctx context.Context, target remote.Target, command string, args []string, input io.Reader) (io.Reader, error) {
	if command != "secret set" && command != "integration set" {
		return input, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return input, nil
	}
	if command == "secret set" {
		if !secrets.ValidName(args[1]) {
			return nil, fmt.Errorf("%q must be an UPPER_CASE name", args[1])
		}
		value, err := readSecretValue(a)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(value), nil
	}
	def, ok := integration.Lookup(args[0])
	if !ok {
		return nil, fmt.Errorf("no integration named %q; there are %s", args[0], strings.Join(integration.Names(), ", "))
	}
	current, err := a.targetIntegrationFields(ctx, target, def)
	if err != nil {
		return nil, err
	}
	values, err := readIntegrationValues(a, def, current)
	if err != nil {
		return nil, err
	}
	var payload strings.Builder
	for _, field := range sortedKeys(values) {
		if strings.ContainsAny(values[field], "\r\n") {
			return nil, errors.New("integration values must each fit on one line")
		}
		fmt.Fprintf(&payload, "%s=%s\n", field, values[field])
	}
	return strings.NewReader(payload.String()), nil
}

// The list contract contains public values and presence markers, never secret
// values. Omitted answers leave the target's existing values untouched.
func (a *app) targetIntegrationFields(ctx context.Context, target remote.Target, def integration.Definition) (map[string]string, error) {
	var out bytes.Buffer
	args := append(a.targetStateArgs(), "integration", "list", "--json")
	if err := targetTransport().Run(ctx, target, args, nil, &boundedBuffer{Buffer: &out, Max: 1 << 20}, a.stderr); err != nil {
		return nil, fmt.Errorf("read target integration status: %w", err)
	}
	var report struct {
		Integrations []integration.Status `json:"integrations"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		return nil, errors.New("target returned invalid integration status")
	}
	for _, status := range report.Integrations {
		if status.Name != def.Name {
			continue
		}
		current := make(map[string]string)
		for _, field := range def.Fields {
			value := status.Fields[field.Name]
			if value == "" {
				continue
			}
			if field.Secret {
				value = "(set)"
			}
			current[field.Name] = value
		}
		return current, nil
	}
	return nil, errors.New("target integration status is incomplete; upgrade the target")
}
