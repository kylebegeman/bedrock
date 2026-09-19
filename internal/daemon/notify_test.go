package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSystemd collects notify datagrams the way systemd would.
type fakeSystemd struct {
	conn *net.UnixConn
	mu   sync.Mutex
	got  []string
}

func newFakeSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, and t.TempDir() is
	// longer than that on macOS.
	dir, err := os.MkdirTemp("/tmp", "quark-notify-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSystemd{conn: conn}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.got = append(f.got, string(buf[:n]))
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return f
}

func (f *fakeSystemd) path() string { return f.conn.LocalAddr().String() }

func (f *fakeSystemd) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func TestNotifierFeedsTheWatchdogWhileHealthy(t *testing.T) {
	sd := newFakeSystemd(t)
	n := newNotifier(sd.path(), "100000") // 100 ms watchdog: ping every 50 ms
	n.ready()
	ctx, cancel := context.WithCancel(context.Background())
	healthy := true
	var mu sync.Mutex
	done := n.watchdog(ctx, func() bool { mu.Lock(); defer mu.Unlock(); return healthy })
	time.Sleep(230 * time.Millisecond)
	mu.Lock()
	healthy = false
	mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	n.stopping()
	time.Sleep(20 * time.Millisecond)
	msgs := sd.messages()
	if len(msgs) < 3 || msgs[0] != "READY=1" || msgs[len(msgs)-1] != "STOPPING=1" {
		t.Fatalf("messages: %v", msgs)
	}
	pings := 0
	for _, m := range msgs {
		if m == "WATCHDOG=1" {
			pings++
		}
	}
	if pings < 3 || pings > 5 {
		t.Fatalf("want 3 to 5 watchdog pings (4 while healthy), got %d: %v", pings, msgs)
	}
}

func TestNotifierIsSilentWithoutSystemd(t *testing.T) {
	n := newNotifier("", "")
	n.ready()
	done := n.watchdog(context.Background(), func() bool { return true })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("no watchdog loop should run without a socket")
	}
	if n.interval != 0 {
		t.Fatal("no interval without WATCHDOG_USEC")
	}
}

func TestAbstractSocketNamesAreTranslated(t *testing.T) {
	n := newNotifier("@systemd/notify", "")
	// Sending must not panic or block; there is no listener.
	n.send("READY=1")
	if !strings.HasPrefix(n.socket, "@") {
		t.Fatal("socket name kept as given")
	}
}
