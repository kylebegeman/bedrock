package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writeMoveHandoff(stateFile string, h Handoff) error {
	file := filepath.Join(filepath.Dir(stateFile), "moves", h.App, "rehearsal.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".handoff-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), file); err != nil {
		return err
	}
	return syncMigrationDir(filepath.Dir(file))
}

// ReadMoveHandoff returns only the exact capture recorded by the operation.
func ReadMoveHandoff(stateFile, app, phase string) (*Handoff, error) {
	if phase == "final" {
		record, err := readMigration(stateFile, app)
		if err != nil {
			return nil, err
		}
		if record == nil || record.Role != "source" || record.Phase != "captured" {
			return nil, errors.New("no completed final capture is recorded")
		}
		return &record.Handoff, nil
	}
	if phase != "rehearsal" {
		return nil, fmt.Errorf("unknown capture phase %q", phase)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(stateFile), "moves", app, "rehearsal.json"))
	if err != nil {
		return nil, err
	}
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	if h.App != app || h.Phase != "rehearsal" {
		return nil, errors.New("rehearsal handoff identity changed")
	}
	return &h, nil
}

// forgetMigration follows explicit app removal, after every app writer was
// removed. A stale fence must not prevent a later app from reusing its name.
func forgetMigration(stateFile, app string) error {
	file := migrationFile(stateFile, app)
	if err := os.Remove(file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncMigrationDir(filepath.Dir(file))
}
