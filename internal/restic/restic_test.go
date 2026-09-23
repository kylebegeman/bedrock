package restic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
)

const summary = `{"message_type":"summary"}` + "\n"

// runner is a Runner on the stand-in, with a password and credentials a
// shell would trip over if they were quoted wrong.
func runner(t *testing.T) Runner {
	t.Helper()
	e, err := docker.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return Runner{Engine: e, Owner: "test", Repo: Repo{Repository: "s3:http://store/lane-test", Password: "pa'ss word", Env: []string{"AWS_ACCESS_KEY_ID=id", "AWS_SECRET_ACCESS_KEY=s3cret"}}}
}

// polls counts how often the stand-in was asked about restic's container.
func polls(fake *dockertest.Server) int {
	n := 0
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "GET /containers/bedrock-test-restic") && strings.HasSuffix(call, "/json") {
			n++
		}
	}
	return n
}

func TestRunReadsTheOutputAndRemovesTheContainerOnceAfterItExits(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "after it exits"
		if cancelled {
			name = "when the wait is cancelled"
		}
		t.Run(name, func(t *testing.T) {
			fake := dockertest.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.OnCreate = func(c dockertest.Container) {
				fake.Update(c.Name, func(c *dockertest.Container) {
					c.Logs = summary
					if !cancelled {
						c.ExitAfter = 2
					}
				})
			}
			if cancelled {
				go func() {
					for polls(fake) < 2 {
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			lines, err := runner(t).run(ctx, nil, "snapshots", "--json")
			if cancelled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("a cancelled wait should say so: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(lines, strings.TrimSpace(summary)) {
				t.Fatalf("the container's output was not read: %q", lines)
			}
			deletes := 0
			for _, c := range fake.Calls() {
				if strings.HasPrefix(c, "DELETE ") {
					deletes++
				}
			}
			if deletes != 1 {
				t.Fatalf("the container was removed %d times: %v", deletes, fake.Calls())
			}
			poll := fake.Last(func(c string) bool { return strings.Contains(c, "/containers/") && strings.HasSuffix(c, "/json") })
			logs := fake.Last(func(c string) bool { return strings.HasSuffix(c, "/logs") })
			removal := fake.Last(func(c string) bool { return strings.HasPrefix(c, "DELETE ") })
			if !(poll < logs && logs < removal) {
				t.Fatalf("output read and removal must follow the last poll: %v", fake.Calls())
			}
		})
	}
}

func TestTheRepositoryPasswordNeverReachesTheContainersEnvironment(t *testing.T) {
	fake := dockertest.New(t)
	var created dockertest.Container
	var mounted string
	resticSaw := func(c dockertest.Container) {
		created = c
		for _, bind := range c.HostConfig.Binds {
			if src, dst, _ := strings.Cut(bind, ":"); strings.HasPrefix(dst, secretsInside) {
				data, err := os.ReadFile(filepath.Join(src, "env"))
				if err != nil {
					t.Errorf("the secrets file isn't there while the container is: %v", err)
				}
				mounted = src
				if info, err := os.Stat(filepath.Join(src, "env")); err == nil && info.Mode().Perm() != 0o600 {
					t.Errorf("the secrets file is %v, not 0600", info.Mode().Perm())
				}
				if !strings.Contains(string(data), "AWS_SECRET_ACCESS_KEY='s3cret'") {
					t.Errorf("the secrets file lacks the credentials: %q", data)
				}
			}
		}
	}
	fake.OnCreate = func(c dockertest.Container) {
		resticSaw(c)
		fake.Update(c.Name, func(c *dockertest.Container) { c.Logs, c.ExitAfter = summary, 1 })
	}
	lines, err := runner(t).run(context.Background(), nil, "snapshots", "--json")
	if err != nil {
		t.Fatalf("%v %q", err, lines)
	}
	for _, kv := range created.Env {
		if strings.Contains(kv, "pa'ss word") || strings.Contains(kv, "s3cret") {
			t.Fatalf("a secret is in the container's environment: %q", kv)
		}
	}
	if mounted == "" || created.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("no secrets mount, or restic isn't started through the shell that reads it: %+v", created)
	}
	if _, err := os.Stat(mounted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the secrets file outlived the run: %v", err)
	}
}

func TestTheSecretsFileGivesAShellTheValuesAsTheyWere(t *testing.T) {
	values := []string{"plain", "pa'ss word", `back\slash "and" $dollar`, "new\nline", "trailing'"}
	var pairs []string
	for i, v := range values {
		pairs = append(pairs, "V"+string(rune('A'+i))+"="+v)
	}
	dir, err := secretsFile(pairs)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for i, want := range values {
		name := "V" + string(rune('A'+i))
		out, err := exec.Command("/bin/sh", "-c", `set -a && . "$1" && set +a && printf %s "$`+name+`"`, "sh", filepath.Join(dir, "env")).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Fatalf("%s: the shell read %q, not %q", name, out, want)
		}
	}
}
