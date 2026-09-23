package docker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
)

func connect(t *testing.T) (*Engine, *dockertest.Server) {
	t.Helper()
	fake := dockertest.New(t)
	e, err := Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, fake
}

func TestAnIsolatedContainerKeepsNothingItDidntAskFor(t *testing.T) {
	e, fake := connect(t)
	spec := Spec{Name: "bedrock-a-web-r1", Image: "img", Labels: map[string]string{LabelApp: "a"}, Isolated: true,
		Capabilities: []string{"NET_BIND_SERVICE"}, ReadOnly: true, Tmpfs: []string{"/app/.cache"}, Restart: true}
	if err := e.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	c, ok := fake.Container("bedrock-a-web-r1")
	if !ok || !c.Running {
		t.Fatalf("not created and started: %+v", c)
	}
	h := c.HostConfig
	switch {
	// Docker's client sends capabilities in their CAP_ form.
	case !slices.Equal(h.CapDrop, []string{"ALL"}), !slices.Equal(h.CapAdd, []string{"CAP_NET_BIND_SERVICE"}):
		t.Fatalf("capabilities: drop %v, add %v", h.CapDrop, h.CapAdd)
	case !h.ReadonlyRootfs || h.Tmpfs["/tmp"] == "" || h.Tmpfs["/app/.cache"] == "":
		t.Fatalf("root filesystem: read-only %v, tmpfs %v", h.ReadonlyRootfs, h.Tmpfs)
	case !slices.Contains(h.SecurityOpt, "no-new-privileges"):
		t.Fatalf("security options %v", h.SecurityOpt)
	case h.PidsLimit == nil || *h.PidsLimit != DefaultPidsLimit:
		t.Fatalf("pids limit %v", h.PidsLimit)
	case h.RestartPolicy.Name != "unless-stopped":
		t.Fatalf("restart policy %q", h.RestartPolicy.Name)
	case c.Labels[LabelOwner] != OwnerValue || c.Labels[LabelApp] != "a":
		t.Fatalf("labels %v", c.Labels)
	}
}

func TestAPrivilegedContainerIsNotIsolated(t *testing.T) {
	e, fake := connect(t)
	if err := e.Run(context.Background(), Spec{Name: "bedrock-a-runner-r1", Image: "img", Privileged: true, Isolated: true}); err != nil {
		t.Fatal(err)
	}
	c, _ := fake.Container("bedrock-a-runner-r1")
	if !c.HostConfig.Privileged || len(c.HostConfig.CapDrop) != 0 || len(c.HostConfig.SecurityOpt) != 0 {
		t.Fatalf("host config %+v", c.HostConfig)
	}
}

func TestRunStartsAStoppedContainerRatherThanMakingAnother(t *testing.T) {
	e, fake := connect(t)
	fake.Add(dockertest.Container{Name: "bedrock-a-web-r1", Labels: map[string]string{LabelOwner: OwnerValue}})
	if err := e.Run(context.Background(), Spec{Name: "bedrock-a-web-r1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := fake.Container("bedrock-a-web-r1"); !c.Running {
		t.Fatal("the stopped container wasn't started")
	}
	if fake.Last(func(c string) bool { return strings.HasSuffix(c, "/containers/create") }) >= 0 {
		t.Fatalf("a second container was made: %v", fake.Calls())
	}
}

func TestOnlyWhatBedrockMadeIsItsOwn(t *testing.T) {
	e, fake := connect(t)
	fake.Add(dockertest.Container{Name: "bedrock-a-web-r1", Running: true, Labels: map[string]string{LabelOwner: OwnerValue, LabelApp: "a"}})
	fake.Add(dockertest.Container{Name: "someone-elses", Running: true, Labels: map[string]string{}})
	owned, err := e.Owned(context.Background())
	if err != nil || len(owned) != 1 || owned[0].Name != "bedrock-a-web-r1" || !owned[0].Running || owned[0].Labels[LabelApp] != "a" {
		t.Fatalf("owned %+v %v", owned, err)
	}
	unowned, err := e.Unowned(context.Background())
	if err != nil || !slices.Equal(unowned, []string{"someone-elses"}) {
		t.Fatalf("unowned %v %v", unowned, err)
	}
}

func TestRemovingAContainerThatIsGoneIsFine(t *testing.T) {
	e, fake := connect(t)
	if err := e.Remove(context.Background(), "never-was", time.Second); err != nil {
		t.Fatal(err)
	}
	fake.Add(dockertest.Container{Name: "bedrock-a-web-r1", Running: true})
	if err := e.Remove(context.Background(), "bedrock-a-web-r1", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Container("bedrock-a-web-r1"); ok {
		t.Fatal("the container is still there")
	}
}
