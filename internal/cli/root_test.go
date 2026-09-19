package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kylebegeman/quark/internal/version"
)

func TestVersionPrintsThisBuild(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	if got, want := out.String(), version.Current().String()+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersionJSONIsOneObject(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main([]string{"version", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	var info version.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("not JSON: %v: %q", err, out.String())
	}
	if info != version.Current() {
		t.Fatalf("got %+v, want %+v", info, version.Current())
	}
}

func TestUnknownCommandFailsOnce(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main([]string{"nope"}, &out, &errOut); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if n := strings.Count(errOut.String(), "unknown command"); n != 1 {
		t.Fatalf("error printed %d times: %q", n, errOut.String())
	}
}
