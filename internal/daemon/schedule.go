package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kylebegeman/bedrock/internal/api"
	"github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// MachineBackupSchedule is when the machine's own state is backed up.
const MachineBackupSchedule = "30 3 * * *"

// Scheduler starts backups and drills when they are due, one at a time,
// as operations with receipts like any other.
type Scheduler struct {
	Store   *state.Store
	Secrets *secrets.Store
	Server  *api.Server
	Log     func(format string, args ...any)
	// Started is when this daemon came up; the machine backup counts
	// from there when it has never run.
	Started time.Time

	mu       sync.Mutex
	busy     bool
	warnedAt time.Time
}

// Tick runs whatever is due at now. A tick that finds the previous one
// still running does nothing.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	active, err := s.Store.ActiveRevisions(ctx)
	if err != nil {
		s.Log("schedule: %v", err)
		return
	}
	if _, err := integration.LoadStorage(s.Secrets); err != nil {
		// Nothing can be backed up until storage is set; say so once an
		// hour, only when there is something that wants backing up.
		if len(active) > 0 && now.Sub(s.warnedAt) >= time.Hour {
			s.warnedAt = now
			s.Log("schedule: no backups run until storage is set: bedrock integration set storage")
		}
		return
	}
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil || !m.BackedUp() {
			continue
		}
		if s.due(ctx, rev.App, state.BackupRunBackup, m.BackupSchedule(), s.firstDeploy(ctx, rev.App), now) {
			s.run(ctx, app.BackupKind, app.BackupInput{App: rev.App}, "backup "+rev.App)
		}
		if good, _ := s.Store.LastGoodBackupRun(ctx, rev.App, state.BackupRunBackup); good != nil {
			if s.due(ctx, rev.App, state.BackupRunDrill, m.DrillSchedule(), good.StartedAt, now) {
				s.run(ctx, app.DrillKind, app.DrillInput{App: rev.App}, "drill "+rev.App)
			}
		}
	}
	if s.due(ctx, manifest.ReservedApp, state.BackupRunBackup, MachineBackupSchedule, s.Started, now) {
		s.run(ctx, app.BackupKind, app.BackupInput{App: manifest.ReservedApp}, "backup of the machine")
	}
}

// due says whether a schedule has come round since the last run of a
// kind, or since a starting point when there was none.
func (s *Scheduler) due(ctx context.Context, appName, kind, schedule string, since, now time.Time) bool {
	sched, err := manifest.ParseSchedule(schedule)
	if err != nil {
		return false
	}
	if last, err := s.Store.LastBackupRun(ctx, appName, kind); err == nil && last != nil && last.StartedAt.After(since) {
		since = last.StartedAt
	}
	if since.IsZero() {
		return false
	}
	next := sched.Next(since)
	return !next.After(now)
}

// firstDeploy is when an app first arrived, so a new app's first backup
// waits for the schedule rather than running the moment it is deployed.
func (s *Scheduler) firstDeploy(ctx context.Context, appName string) time.Time {
	revs, err := s.Store.Revisions(ctx, appName)
	if err != nil || len(revs) == 0 {
		return time.Time{}
	}
	return revs[len(revs)-1].CreatedAt
}

func (s *Scheduler) run(ctx context.Context, kind string, input any, what string) {
	raw, err := json.Marshal(input)
	if err != nil {
		return
	}
	s.Log("scheduled %s starting", what)
	receipt, err := s.Server.RunLocked(ctx, kind, raw, func(ev kernel.Event) {
		if ev.Type == kernel.EventStepFinished && ev.Step != nil && ev.Step.Error != "" {
			s.Log("scheduled %s: %s: %s", what, ev.Step.Name, ev.Step.Error)
		}
	})
	switch {
	case err != nil:
		s.Log("scheduled %s: %v", what, err)
	case receipt != nil:
		s.Log("scheduled %s: %s (%s)", what, receipt.Status, receipt.ID)
	}
}
