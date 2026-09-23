package host

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/kernel"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the code does now")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s changed:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func viewText(v *kernel.PlanView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s on %s, %s\n", v.Kind, v.Target, v.Recovery)
	for _, st := range v.Steps {
		fmt.Fprintf(&b, "%2d %s: %s", st.Index+1, st.Name, st.Change)
		if st.Note != "" {
			fmt.Fprintf(&b, " (%s)", st.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestTheSetupPlansStayWhatTheyWere(t *testing.T) {
	for name, machine := range map[string]func(*testing.T) *fakeMachine{"fresh": freshUbuntu, "set-up": setUpBox} {
		t.Run(name, func(t *testing.T) {
			m := machine(t)
			view, _ := planSteps(t, m, Profile{Hostname: "box", SwapGiB: 4, Timezone: "UTC"})
			golden(t, "plan-setup-"+name, viewText(view))
		})
	}
}

func TestTheDoctorSaysWhatItSaid(t *testing.T) {
	var b strings.Builder
	for _, machine := range []struct {
		name string
		make func(*testing.T) *fakeMachine
	}{{"fresh", freshUbuntu}, {"set-up", setUpBox}} {
		fmt.Fprintf(&b, "%s\n", machine.name)
		for _, r := range Diagnose(Gather(context.Background(), machine.make(t).env(), "")) {
			fmt.Fprintf(&b, "  %s %s: %s", r.Verdict, r.Name, r.Detail)
			if r.Fix != "" {
				fmt.Fprintf(&b, " (fix: %s)", r.Fix)
			}
			b.WriteString("\n")
		}
	}
	golden(t, "doctor", b.String())
}

func TestTheCloudflareOnlySetupPlanStaysWhatItWas(t *testing.T) {
	m := setUpBox(t)
	view, _ := planStepsWith(t, m, Setup{CloudflareRanges: rangesFrom(testRanges, nil)}, Profile{Hostname: "box", SwapGiB: 4, Timezone: "UTC", WebFrom: WebFromCloudflare})
	golden(t, "plan-setup-cloudflare", viewText(view))
}
