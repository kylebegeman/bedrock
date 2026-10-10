package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type machineRecoveryReservation struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Snapshot   string    `json:"snapshot"`
	ReservedAt time.Time `json:"reserved_at"`
}

func machineRecoveryGatePath(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), "locks", "machine-recovery.json")
}

func readMachineRecoveryReservation(stateFile string) (*machineRecoveryReservation, error) {
	path := machineRecoveryGatePath(stateFile)
	if err := recoveryPlainFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var gate machineRecoveryReservation
	if json.Unmarshal(data, &gate) != nil || !recoveryIDPattern.MatchString(gate.ID) || !recoverySourcePattern.MatchString(gate.Source) || !fullSnapshotID.MatchString(gate.Snapshot) {
		return nil, errors.New("invalid machine recovery reservation; inspect the protected reservation before admitting writes")
	}
	return &gate, nil
}

// CheckMachineRecoveryAccess is read-only and is used by registry planning
// and by the shared machine operation lock before an apply. A reservation
// survives reboot and permits only its original recovery and cutover.
func CheckMachineRecoveryAccess(stateFile, kind string, raw json.RawMessage) error {
	gate, err := readMachineRecoveryReservation(stateFile)
	if err != nil || gate == nil {
		return err
	}
	if kind == RecoverMachineKind {
		var in RecoverMachineInput
		if json.Unmarshal(raw, &in) == nil && in.ID == gate.ID && in.Source == gate.Source && !in.Rehearsal && snapshotPrefix.MatchString(in.Snapshot) && strings.HasPrefix(gate.Snapshot, in.Snapshot) {
			return nil
		}
	}
	if kind == RecoverCutoverKind || kind == RecoverAbandonKind {
		var in RecoverCutoverInput
		if json.Unmarshal(raw, &in) == nil && in.ID == gate.ID {
			return nil
		}
	}
	return fmt.Errorf("machine is reserved by recovery %s from %s; inspect bedrock recover report %s and retry that recovery or cut over after readiness", gate.ID, gate.Source, gate.ID)
}

// CheckMachineRecoveryWriteAccess excludes direct data commands and jobs
// while a real recovery owns the machine. Read-only reports remain usable.
func CheckMachineRecoveryWriteAccess(stateFile string) error {
	return CheckMachineRecoveryAccess(stateFile, "data.write", nil)
}

func (x *machineRecovering) reserve(rec *machineRecoveryRecord) error {
	if x.in.Rehearsal {
		return nil
	}
	stateFile := x.r.Store.Path()
	gate, err := readMachineRecoveryReservation(stateFile)
	if err != nil {
		return err
	}
	if gate != nil {
		if gate.ID != x.in.ID || gate.Source != x.in.Source || gate.Snapshot != rec.Snapshot.ID {
			return errors.New("the machine is already reserved by a different recovery")
		}
		return nil
	}
	path := machineRecoveryGatePath(stateFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	gate = &machineRecoveryReservation{ID: x.in.ID, Source: x.in.Source, Snapshot: rec.Snapshot.ID, ReservedAt: time.Now().UTC()}
	data, err := json.Marshal(gate)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}

func (x *machineRecovering) releaseReservation() error {
	if x.in.Rehearsal {
		return nil
	}
	path := machineRecoveryGatePath(x.r.Store.Path())
	gate, err := readMachineRecoveryReservation(x.r.Store.Path())
	if err != nil {
		return err
	}
	if gate == nil {
		return nil
	}
	if gate.ID != x.in.ID {
		return errors.New("cannot release another recovery's machine reservation")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}
