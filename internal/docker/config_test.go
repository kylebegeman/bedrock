package docker

import (
	"slices"
	"testing"
)

// An isolated container gives up every capability and privilege it did
// not ask for, and is bounded; a privileged one keeps only what it asked.
func TestAContainerIsGivenOnlyWhatItsSpecAsks(t *testing.T) {
	hc, exposed, err := hostConfigFor(Spec{Isolated: true, ReadOnly: true, Capabilities: []string{"NET_BIND_SERVICE"}, Tmpfs: []string{"/run"}, Publish: []string{"127.0.0.1:5000:5000"}, Restart: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hc.CapDrop, []string{"ALL"}) || !slices.Equal(hc.CapAdd, []string{"NET_BIND_SERVICE"}) || !slices.Equal(hc.SecurityOpt, []string{"no-new-privileges"}) {
		t.Fatalf("privileges: drop %v add %v opts %v", hc.CapDrop, hc.CapAdd, hc.SecurityOpt)
	}
	if hc.Resources.PidsLimit == nil || *hc.Resources.PidsLimit != DefaultPidsLimit || !hc.ReadonlyRootfs {
		t.Fatalf("limits: %+v", hc.Resources)
	}
	if hc.Tmpfs["/tmp"] != "rw,nosuid" || hc.Tmpfs["/run"] != "rw,nosuid" || hc.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("tmpfs %v restart %v", hc.Tmpfs, hc.RestartPolicy)
	}
	if len(exposed) != 1 || len(hc.PortBindings) != 1 {
		t.Fatalf("ports: %v %v", exposed, hc.PortBindings)
	}
	for port, bindings := range hc.PortBindings {
		if port.String() != "5000/tcp" || bindings[0].HostIP.String() != "127.0.0.1" || bindings[0].HostPort != "5000" {
			t.Fatalf("binding %v %v", port, bindings)
		}
	}

	hc, _, err = hostConfigFor(Spec{Privileged: true, Isolated: true})
	if err != nil {
		t.Fatal(err)
	}
	if hc.CapDrop != nil || hc.SecurityOpt != nil || hc.Resources.PidsLimit != nil || !hc.Privileged {
		t.Fatalf("privileged: %+v", hc)
	}
	hc, _, _ = hostConfigFor(Spec{Privileged: true, PidsLimit: 64})
	if hc.Resources.PidsLimit == nil || *hc.Resources.PidsLimit != 64 {
		t.Fatal("a privileged container keeps the bound it asked for")
	}
	if _, _, err := hostConfigFor(Spec{Publish: []string{"5000"}}); err == nil {
		t.Fatal("a publish without a host port must be refused")
	}
}

func TestEveryContainerIsLabeledAsBedrocks(t *testing.T) {
	c := containerConfigFor(Spec{Image: "x", Labels: map[string]string{LabelApp: "shop"}, NoHealthcheck: true}, nil)
	if c.Labels[LabelOwner] != OwnerValue || c.Labels[LabelApp] != "shop" || c.Healthcheck == nil || c.Healthcheck.Test[0] != "NONE" || c.OpenStdin {
		t.Fatalf("%+v", c)
	}
}
