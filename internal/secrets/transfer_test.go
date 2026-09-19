package secrets

import (
	"strings"
	"testing"
)

func TestTransferIsSealedBoundToDestinationAndIdempotent(t *testing.T) {
	sender, receiver, other := newStore(t), newStore(t), newStore(t)
	sender.EnsureKey()
	recipient, _, _, _ := receiver.EnsureKey()
	other.EnsureKey()
	const token = "fixture-only-secret"
	if _, err := sender.Set("core", "TOKEN", token); err != nil {
		t.Fatal(err)
	}
	sealed, err := sender.Export("core", "TOKEN", "runner", recipient)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, token) || !strings.Contains(sealed, "AGE ENCRYPTED FILE") {
		t.Fatal("not sealed")
	}
	if _, err := other.Import("runner", "TOKEN", strings.NewReader(sealed)); err == nil {
		t.Fatal("wrong host accepted")
	}
	if _, err := receiver.Import("other", "TOKEN", strings.NewReader(sealed)); err == nil {
		t.Fatal("wrong app accepted")
	}
	if _, err := receiver.Import("runner", "OTHER", strings.NewReader(sealed)); err == nil {
		t.Fatal("wrong name accepted")
	}
	v, err := receiver.Import("runner", "TOKEN", strings.NewReader(sealed))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := receiver.Import("runner", "TOKEN", strings.NewReader(sealed))
	if err != nil || v != v2 {
		t.Fatal("replay changed the version")
	}
	values, _, err := receiver.LoadCurrent("runner")
	if err != nil || values["TOKEN"] != token {
		t.Fatal("value not delivered")
	}
	for _, app := range []string{"quark", "../escape", ""} {
		if _, err := sender.Export("core", "TOKEN", app, recipient); err == nil {
			t.Fatal("invalid destination")
		}
		if _, err := receiver.Import(app, "TOKEN", strings.NewReader(sealed)); err == nil {
			t.Fatal("invalid destination")
		}
	}
	if _, err := receiver.Import("runner", "TOKEN", strings.NewReader(strings.Repeat("x", MaxTransferBytes+1))); err == nil {
		t.Fatal("oversized transfer accepted")
	}
}
