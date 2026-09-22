package secrets

import (
	"strings"
	"testing"
	"time"
)

// two stores standing in for two machines, each with its own identity.
func twoMachines(t *testing.T) (source, target *Store, targetRecipient string) {
	t.Helper()
	source = DefaultStore(t.TempDir())
	target = DefaultStore(t.TempDir())
	if _, _, _, err := source.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	recipient, _, _, err := target.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	return source, target, recipient
}

func TestABundleCarriesEverySecretToTheOtherMachine(t *testing.T) {
	source, target, recipient := twoMachines(t)
	for name, value := range map[string]string{
		"DATABASE_URL":       "postgres://app:hunter2@db:5432/notes",
		"NOTES_APP_PASSWORD": "hunter2",
		"API_TOKEN":          "t0ken",
	} {
		if _, err := source.Set("notes", name, value); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := source.ExportBundle("notes", recipient)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing readable may appear in what gets printed and copied.
	for _, secret := range []string{"hunter2", "t0ken", "postgres://"} {
		if strings.Contains(sealed, secret) {
			t.Fatalf("the sealed bundle leaks %q", secret)
		}
	}

	stored, err := target.ImportBundle("notes", strings.NewReader(sealed))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("stored %v, want three", stored)
	}
	got, _, err := target.LoadCurrent("notes")
	if err != nil {
		t.Fatal(err)
	}
	if got["NOTES_APP_PASSWORD"] != "hunter2" || got["DATABASE_URL"] != "postgres://app:hunter2@db:5432/notes" {
		t.Fatalf("the target did not get the values: %v", got)
	}
}

// Importing twice must not churn the version history, so a move that is
// retried costs nothing.
func TestImportingTheSameBundleTwiceStoresNothingTheSecondTime(t *testing.T) {
	source, target, recipient := twoMachines(t)
	if _, err := source.Set("notes", "API_TOKEN", "t0ken"); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.ExportBundle("notes", recipient)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := target.ImportBundle("notes", strings.NewReader(sealed)); err != nil || len(stored) != 1 {
		t.Fatalf("%v %v", stored, err)
	}
	stored, err := target.ImportBundle("notes", strings.NewReader(sealed))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("the second import stored %v", stored)
	}
}

// A bundle is sealed to one machine. Any other machine, including the one
// that made it, must not be able to open it.
func TestABundleOpensOnlyOnTheMachineItWasSealedFor(t *testing.T) {
	source, _, recipient := twoMachines(t)
	if _, err := source.Set("notes", "API_TOKEN", "t0ken"); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.ExportBundle("notes", recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ImportBundle("notes", strings.NewReader(sealed)); err == nil {
		t.Fatal("the source must not be able to open a bundle sealed for the target")
	}
	elsewhere := DefaultStore(t.TempDir())
	if _, _, _, err := elsewhere.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := elsewhere.ImportBundle("notes", strings.NewReader(sealed)); err == nil {
		t.Fatal("a third machine must not be able to open it")
	}
}

// A bundle names its app, so it cannot be imported as a different one.
func TestABundleCannotBeImportedAsAnotherApp(t *testing.T) {
	source, target, recipient := twoMachines(t)
	if _, err := source.Set("notes", "API_TOKEN", "t0ken"); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.ExportBundle("notes", recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportBundle("begamin", strings.NewReader(sealed)); err == nil {
		t.Fatal("a bundle for notes must not import as begamin")
	}
}

func TestBundlesRefuseWhatTheyShould(t *testing.T) {
	source, target, recipient := twoMachines(t)
	if _, err := source.Set("notes", "API_TOKEN", "t0ken"); err != nil {
		t.Fatal(err)
	}
	// The integration store is not an app and never travels.
	if _, err := source.ExportBundle("bedrock", recipient); err == nil {
		t.Fatal("bedrock's own credentials must never be exported")
	}
	if _, err := source.ExportBundle("notes", "not-a-recipient"); err == nil {
		t.Fatal("an invalid recipient must be refused")
	}
	if _, err := source.ExportBundle("nothing-here", recipient); err == nil {
		t.Fatal("an app with no secrets has no bundle")
	}
	for name, body := range map[string]string{
		"empty":     "",
		"not age":   "just some text",
		"truncated": "-----BEGIN AGE ENCRYPTED FILE-----\nnonsense\n",
	} {
		if _, err := target.ImportBundle("notes", strings.NewReader(body)); err == nil {
			t.Fatalf("%s: must be refused", name)
		}
	}
}

// The expiry is what keeps a copied bundle from being useful later.
func TestABundleStopsWorking(t *testing.T) {
	source, target, recipient := twoMachines(t)
	if _, err := source.Set("notes", "API_TOKEN", "t0ken"); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.ExportBundle("notes", recipient)
	if err != nil {
		t.Fatal(err)
	}
	// Ten minutes is the window; well past it the bundle is refused.
	restore := timeNow
	timeNow = func() time.Time { return time.Now().Add(30 * time.Minute) }
	defer func() { timeNow = restore }()
	if _, err := target.ImportBundle("notes", strings.NewReader(sealed)); err == nil {
		t.Fatal("an expired bundle must be refused")
	}
}
