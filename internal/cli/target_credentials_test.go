//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/remote"
)

type credentialPromptOutput struct {
	mu      sync.Mutex
	data    bytes.Buffer
	changed chan struct{}
}

func (w *credentialPromptOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.data.Write(p)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (w *credentialPromptOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

type credentialTestAnswer struct {
	prompt string
	value  string
	secret bool
}

func driveTargetCredentialCommand(t *testing.T, config string, args []string, answers []credentialTestAnswer) (string, string, int) {
	t.Helper()
	master, slave := openCredentialTestPTY(t)
	original := os.Stdin
	os.Stdin = slave
	defer func() { master.Close(); slave.Close(); os.Stdin = original }()
	var stdout bytes.Buffer
	stderr := &credentialPromptOutput{changed: make(chan struct{}, 1)}
	done := make(chan int, 1)
	full := append([]string{"--targets-file", config, "--target", "one"}, args...)
	go func() { done <- Main(full, &stdout, stderr) }()
	finished := false
	defer func() {
		if !finished {
			master.Close()
			slave.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
	}()
	for _, answer := range answers {
		deadline := time.After(3 * time.Second)
		for !strings.Contains(stderr.String(), answer.prompt) {
			select {
			case <-stderr.changed:
			case code := <-done:
				finished = true
				t.Fatalf("command exited before credential prompt: %d %s", code, stderr.String())
			case <-deadline:
				t.Fatalf("credential prompt missing: %q; %s", answer.prompt, stderr.String())
			}
		}
		if answer.secret {
			until := time.Now().Add(3 * time.Second)
			for {
				echo, err := credentialTestEchoEnabled(int(slave.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				if !echo {
					break
				}
				if time.Now().After(until) {
					t.Fatal("credential prompt left terminal echo enabled")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if _, err := master.WriteString(answer.value + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case code := <-done:
		finished = true
		return stdout.String(), stderr.String(), code
	case <-time.After(3 * time.Second):
		t.Fatal("credential command waited for terminal EOF")
		return "", "", 1
	}
}

func TestRemoteSecretTerminalInputIsHiddenAndFinite(t *testing.T) {
	config, record, captured := targetArtifactFixture(t)
	private := "fixture-hidden-secret"
	out, stderr, code := driveTargetCredentialCommand(t, config, []string{"secret", "set", "fixture", "TOKEN"}, []credentialTestAnswer{{"Value (not shown): ", private, true}})
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(captured, "applied-stdin"))
	if err != nil || string(raw) != private {
		t.Fatalf("terminal value was not streamed: %v", err)
	}
	argv, _ := os.ReadFile(record)
	if strings.Contains(string(argv)+out+stderr, private) {
		t.Fatal("terminal secret escaped stdin")
	}
}

func TestRemoteIntegrationTerminalInputKeepsExistingValuesPrivate(t *testing.T) {
	for _, password := range []string{"fixture-hidden-smtp-password", ""} {
		name := "replace-secret"
		if password == "" {
			name = "keep-secret"
		}
		t.Run(name, func(t *testing.T) {
			config, record, captured := targetArtifactFixture(t)
			targets, err := remote.Load(config)
			if err != nil {
				t.Fatal(err)
			}
			binary := targets.Targets["one"].Binary
			script, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			status := `{"integrations":[{"name":"email","set":true,"fields":{"smtp_host":"smtp.example.test","smtp_port":"587","smtp_user":"fixture-user","smtp_password":"(set)","from":"sender@example.test","to":"recipient@example.test"}}]}`
			replacement := " *integration*list*) printf '%s\n' '" + status + "'; exit 0 ;;\n *--plan*)"
			script = []byte(strings.Replace(string(script), " *--plan*)", replacement, 1))
			if err := os.WriteFile(binary, script, 0o700); err != nil {
				t.Fatal(err)
			}
			out, stderr, code := driveTargetCredentialCommand(t, config, []string{"integration", "set", "email"}, []credentialTestAnswer{
				{"SMTP host [smtp.example.test]: ", "", false},
				{"SMTP port (465 for TLS, 587 for STARTTLS) [587]: ", "", false},
				{"SMTP user [fixture-user]: ", "", false},
				{"SMTP password [kept]: ", password, true},
				{"from address [sender@example.test]: ", "", false},
				{"to address (several separated by commas) [recipient@example.test]: ", "", false},
			})
			if code != 0 {
				t.Fatalf("%d %s", code, stderr)
			}
			want := ""
			if password != "" {
				want = "smtp_password=" + password + "\n"
			}
			raw, err := os.ReadFile(filepath.Join(captured, "applied-stdin"))
			if err != nil || string(raw) != want {
				t.Fatalf("blank answers did not keep remote values: %v", err)
			}
			argv, _ := os.ReadFile(record)
			if strings.Contains(string(argv)+out+stderr, "fixture-hidden-smtp-password") || strings.Contains(stderr, "(set)") {
				t.Fatal("remote credential appeared in prompt or arguments")
			}
		})
	}
}

func TestRemoteSecretEmptyTerminalInputIsRefused(t *testing.T) {
	config, record, _ := targetArtifactFixture(t)
	_, stderr, code := driveTargetCredentialCommand(t, config, []string{"secret", "set", "fixture", "TOKEN"}, []credentialTestAnswer{{"Value (not shown): ", "", true}})
	if code == 0 || !strings.Contains(stderr, "nothing entered") {
		t.Fatalf("empty value accepted: %d %s", code, stderr)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("empty secret reached the target")
	}
}
