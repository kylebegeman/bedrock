package app

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/integration"
)

// The mismatched bucket is the whole reason Check exists. Two machines set
// up separately read different buckets, so a target left to its own storage
// finds nothing and reports an app with no data rather than a failed move.
func TestCheckRefusesAHandoffThisMachineCannotRead(t *testing.T) {
	h := &Handoff{App: "dragon-writer", HasData: true, Bucket: "kbquark-dragon-writer", Snapshot: "8406de91"}
	err := h.Check("dragon-writer", &integration.Storage{BucketPrefix: "kbbedrock"})
	if err == nil {
		t.Fatal("a bucket this machine cannot read must be refused")
	}
	for _, want := range []string{"kbquark-dragon-writer", "kbbedrock-dragon-writer", "same storage integration"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error should say %q: %v", want, err)
		}
	}
}

func TestCheckPassesWhenBothMachinesReadTheSameBucket(t *testing.T) {
	h := &Handoff{App: "dragon-writer", HasData: true, Bucket: "kb-dragon-writer", Snapshot: "8406de91"}
	if err := h.Check("dragon-writer", &integration.Storage{BucketPrefix: "kb"}); err != nil {
		t.Fatal(err)
	}
}

// An app with no data needs no bucket, no snapshot and no storage at all.
func TestAnAppWithNoDataMovesWithoutStorage(t *testing.T) {
	h := &Handoff{App: "site", HasData: false}
	if err := h.Check("site", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesTheWrongApp(t *testing.T) {
	h := &Handoff{App: "dragon-writer", HasData: false}
	if err := h.Check("begamin", nil); err == nil {
		t.Fatal("a handoff for another app must be refused")
	}
}

func TestCheckRefusesAnIncompleteHandoff(t *testing.T) {
	st := &integration.Storage{BucketPrefix: "kb"}
	for name, h := range map[string]*Handoff{
		"no app":      {HasData: false},
		"no bucket":   {App: "a", HasData: true, Snapshot: "s"},
		"no snapshot": {App: "a", HasData: true, Bucket: "kb-a"},
	} {
		if err := h.Check("", st); err == nil {
			t.Fatalf("%s: must be refused", name)
		}
	}
	withData := &Handoff{App: "a", HasData: true, Bucket: "kb-a", Snapshot: "s"}
	if err := withData.Check("", nil); err == nil {
		t.Fatal("no storage on this machine must be refused when there is data")
	}
}

// A handoff is written on one machine and read on another, so it has to
// survive the trip exactly, and anything that isn't one must be refused
// rather than read as an empty one.
func TestAHandoffSurvivesBeingWrittenAndRead(t *testing.T) {
	h := &Handoff{
		App: "dragon-writer", From: "personal-vps", Bucket: "kb-dragon-writer",
		Snapshot: "8406de91", HasData: true,
		Hosts: []string{"dragonwriter.begam.in"}, Repo: "github.com/kylebegeman/dragon-writer",
		Revision: "20260921-012329",
	}
	var buf bytes.Buffer
	if err := WriteHandoff(&buf, h); err != nil {
		t.Fatal(err)
	}
	back, err := ReadHandoff(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.App != h.App || back.Bucket != h.Bucket || back.Snapshot != h.Snapshot ||
		back.Revision != h.Revision || back.Repo != h.Repo || len(back.Hosts) != 1 || back.Hosts[0] != h.Hosts[0] {
		t.Fatalf("round trip lost something: %+v", back)
	}

	for name, body := range map[string]string{
		"empty":        "",
		"not json":     "handoff",
		"no app":       `{"bucket":"kb-a"}`,
		"another kind": `{"app":"a","unexpected":1}`,
	} {
		if _, err := ReadHandoff(strings.NewReader(body)); err == nil {
			t.Fatalf("%s: must be refused", name)
		}
	}
}

// A handoff carries nothing that would be dangerous to print, because the
// whole point is that it can be read and copied by hand.
func TestAHandoffCarriesNoSecrets(t *testing.T) {
	h := &Handoff{
		App: "a", From: "m", Bucket: "kb-a", Snapshot: "s", HasData: true,
		Hosts: []string{"a.example.com"}, Repo: "github.com/x/y", Revision: "r",
	}
	var buf bytes.Buffer
	if err := WriteHandoff(&buf, h); err != nil {
		t.Fatal(err)
	}
	// Every field the storage integration holds that must never travel.
	for _, forbidden := range []string{"password", "key_id", "key\"", "token", "secret"} {
		if strings.Contains(strings.ToLower(buf.String()), forbidden) {
			t.Fatalf("a handoff must not carry %q: %s", forbidden, buf.String())
		}
	}
}

func TestErrNoSnapshotIsRecognisable(t *testing.T) {
	if !errors.Is(ErrNoSnapshot, ErrNoSnapshot) {
		t.Fatal("sanity")
	}
}
