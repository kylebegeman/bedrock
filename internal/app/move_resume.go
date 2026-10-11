package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

type MoveResume struct{ Store *state.Store }
type MoveResumeInput struct {
	App       string `json:"app"`
	Migration string `json:"migration"`
	// TargetStopped is a deliberate operator assertion. A disconnected
	// source cannot prove the destination stopped or recover its new writes.
	TargetStopped bool `json:"target_stopped"`
}

func (MoveResume) Kind() string { return MoveResumeKind }
func (r MoveResume) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in MoveResumeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if err := checkReleaseStoreAccess(ctx, r.Store, in.App); err != nil {
		return nil, err
	}
	if !in.TargetStopped {
		return nil, errors.New("resuming source requires --target-stopped: stop the target first; source data excludes all writes made there")
	}
	record, err := readMigration(r.Store.Path(), in.App)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Role != "source" || record.Handoff.Migration != in.Migration {
		record, err = readResumedMigration(r.Store.Path(), in.App, in.Migration)
		if err != nil || record == nil {
			return nil, errors.New("no matching source migration fence to resume")
		}
	}
	plan := &kernel.Plan{Target: in.App, Recovery: kernel.Resume, Steps: []kernel.Step{{Name: "resume", Change: fmt.Sprintf("deliberately resume source %s for %s, retaining source revision %s; destination writes are not recovered", record.Handoff.From, in.Migration, record.Handoff.Revision), Apply: func(ctx context.Context, out io.Writer) error {
		unlock, err := lockAppData(ctx, r.Store.Path(), in.App, true)
		if err != nil {
			return err
		}
		defer unlock()
		current, err := readMigration(r.Store.Path(), in.App)
		if err != nil {
			return err
		}
		if current == nil {
			return nil
		}
		if !sameMigration(current.Handoff, record.Handoff) {
			return errors.New("source migration changed")
		}
		e, err := docker.Connect(ctx)
		if err != nil {
			return err
		}
		defer e.Close()
		if err := resumeMigration(ctx, e, r.Store.Path(), current); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s resumed deliberately; source data contains no destination writes\n", in.App)
		return nil
	}}}}
	return guardReleaseRestoreSteps(plan, r.Store), nil
}
func resumeMigration(ctx context.Context, e migrationEngine, stateFile string, record *migrationRecord) error {
	writers := append([]migrationWriter(nil), record.Writers...)
	sort.SliceStable(writers, func(i, j int) bool { return writers[i].Objects && !writers[j].Objects })
	for _, w := range writers {
		info, err := e.Inspect(ctx, w.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if w.Running && !info.Running {
			if err := e.Start(ctx, w.ID); err != nil {
				return err
			}
		}
		if err := e.SetRestartPolicy(ctx, w.ID, w.Policy); err != nil {
			return err
		}
	}
	if err := writeMigrationRecord(resumedMigrationFile(stateFile, record.Handoff.App, record.Handoff.Migration), record); err != nil {
		return err
	}
	file := migrationFile(stateFile, record.Handoff.App)
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncMigrationDir(filepath.Dir(file))
}

func MigrationResumeInput(stateFile, app string, targetStopped bool) (MoveResumeInput, error) {
	record, err := readMigration(stateFile, app)
	if err != nil {
		return MoveResumeInput{}, err
	}
	if record == nil || record.Role != "source" {
		return MoveResumeInput{}, errors.New("no source migration fence")
	}
	return MoveResumeInput{App: app, Migration: record.Handoff.Migration, TargetStopped: targetStopped}, nil
}

func resumedMigrationFile(stateFile, app, migration string) string {
	return filepath.Join(filepath.Dir(stateFile), "moves", app, "resumed", migration+".json")
}

func readResumedMigration(stateFile, app, migration string) (*migrationRecord, error) {
	if !migrationIDPattern.MatchString(migration) {
		return nil, errors.New("invalid migration ID")
	}
	data, err := os.ReadFile(resumedMigrationFile(stateFile, app, migration))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record migrationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if record.Handoff.App != app || record.Handoff.Migration != migration {
		return nil, errors.New("resumed migration identity changed")
	}
	return &record, nil
}
