package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kylebegeman/helios/internal/compat"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/state"
	"github.com/kylebegeman/helios/internal/version"
)

func probeCapabilities(ctx context.Context, env Env, path string) (compat.Info, error) {
	out, err := env.Run(ctx, path, "capabilities", "--json")
	if err != nil {
		return compat.Info{}, fmt.Errorf("upgrade preflight: %s cannot report supported contracts: %w", path, err)
	}
	var info compat.Info
	if err := json.Unmarshal([]byte(lastLine(out)), &info); err != nil || info.APIVersion == 0 {
		return info, fmt.Errorf("upgrade preflight: %s does not report valid capabilities", path)
	}
	required := []string{"kernel.plan.v1", "kernel.apply.v1", "kernel.expect.v1", "state.migrations.v1", "upgrade.checkpoint.v1"}
	for _, name := range required {
		found := false
		for _, have := range info.Capabilities {
			if have == name {
				found = true
				break
			}
		}
		if !found {
			return info, fmt.Errorf("upgrade preflight: candidate lacks %s", name)
		}
	}
	return info, nil
}

type upgradeCheckpoint struct {
	OperationID  string       `json:"operation_id"`
	Target       version.Info `json:"target"`
	Path         string       `json:"path"`
	StateVersion int          `json:"state_version"`
	CreatedAt    time.Time    `json:"created_at"`
}

func checkpointUpgrade(ctx context.Context, store *state.Store, env Env, target version.Info) (string, error) {
	metadata := env.Path(LibDir + "/state-checkpoint.json")
	if kernel.Attempt(ctx) > 1 {
		data, err := os.ReadFile(metadata)
		if err == nil {
			var previous upgradeCheckpoint
			if err := json.Unmarshal(data, &previous); err != nil {
				return "", err
			}
			if previous.OperationID != kernel.OperationID(ctx) {
				return "", errors.New("state checkpoint belongs to another upgrade operation")
			}
			if identity(previous.Target) != identity(target) {
				return "", errors.New("state checkpoint belongs to another candidate")
			}
			if _, err := os.Stat(previous.Path); err != nil {
				return "", fmt.Errorf("previous upgrade state checkpoint is unavailable: %w", err)
			}
			if err := store.Checkpoint(ctx, previous.Path); err != nil {
				return "", err
			}
			return previous.Path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	dir := env.Path(LibDir + "/checkpoints")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "state-*.db")
	if err != nil {
		return "", err
	}
	path := file.Name()
	file.Close()
	os.Remove(path)
	if err := store.Checkpoint(ctx, path); err != nil {
		return "", err
	}
	v, err := store.Version(ctx)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(upgradeCheckpoint{OperationID: kernel.OperationID(ctx), Target: target, Path: path, StateVersion: v, CreatedAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(metadata+".tmp", append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(metadata+".tmp", metadata); err != nil {
		return "", err
	}
	return path, nil
}
