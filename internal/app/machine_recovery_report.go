package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// MachineRecoveryReport is the public proof of a recovery. It excludes
// private identities, ciphertext, manifests and integration credentials.
type MachineRecoveryReport struct {
	SchemaVersion  int               `json:"schema_version"`
	ID             string            `json:"id"`
	Source         string            `json:"source"`
	Snapshot       string            `json:"snapshot"`
	Rehearsal      bool              `json:"rehearsal"`
	Status         string            `json:"status"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     time.Time         `json:"finished_at,omitempty"`
	AppSnapshots   map[string]string `json:"app_snapshots"`
	PrivateReady   map[string]bool   `json:"private_ready"`
	CutoverStarted bool              `json:"cutover_started"`
	Cutover        bool              `json:"cutover"`
	SourceVersion  string            `json:"source_version"`
	SourceOS       string            `json:"source_os"`
	SourceArch     string            `json:"source_arch"`
}

func ReadMachineRecoveryReport(stateDir, id string) (*MachineRecoveryReport, error) {
	if !recoveryIDPattern.MatchString(id) {
		return nil, errors.New("invalid recovery ID")
	}
	path := filepath.Join(RecoveryDirectory(stateDir, id), "recovery-record.json")
	if err := recoveryPlainFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec machineRecoveryRecord
	if json.Unmarshal(data, &rec) != nil || rec.Input.ID != id {
		return nil, errors.New("invalid recovery record")
	}
	status := "incomplete"
	if !rec.FinishedAt.IsZero() {
		status = "private-ready"
		if rec.Input.Rehearsal {
			status = "rehearsed"
		}
	}
	if rec.CutoverStarted {
		status = "cutover-incomplete"
	}
	if !rec.AbandonedAt.IsZero() {
		status = "abandoned"
	}
	if rec.Cutover {
		status = "live"
	}
	return &MachineRecoveryReport{SchemaVersion: 1, ID: id, Source: rec.Input.Source, Snapshot: rec.Snapshot.ID,
		Rehearsal: rec.Input.Rehearsal, Status: status, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		AppSnapshots: rec.Catalog.AppSnapshots, PrivateReady: rec.Ready, CutoverStarted: rec.CutoverStarted, Cutover: rec.Cutover,
		SourceVersion: rec.Catalog.Version, SourceOS: rec.Catalog.OS, SourceArch: rec.Catalog.Arch}, nil
}
