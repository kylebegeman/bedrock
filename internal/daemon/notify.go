package daemon

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// notifier speaks systemd's notify protocol: READY, WATCHDOG and STOPPING
// datagrams to $NOTIFY_SOCKET. Without that variable it does nothing, so
// the daemon runs the same way under systemd, in a test, or by hand.
type notifier struct {
	socket   string
	interval time.Duration // half of WATCHDOG_USEC; zero means no watchdog
}

func notifierFromEnv() notifier {
	return newNotifier(os.Getenv("NOTIFY_SOCKET"), os.Getenv("WATCHDOG_USEC"))
}

func newNotifier(socket, watchdogUsec string) notifier {
	n := notifier{socket: socket}
	if usec, err := strconv.ParseInt(watchdogUsec, 10, 64); err == nil && usec > 0 {
		n.interval = time.Duration(usec) * time.Microsecond / 2
	}
	return n
}

func (n notifier) send(state string) {
	if n.socket == "" {
		return
	}
	name := n.socket
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:] // abstract namespace
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

func (n notifier) ready()    { n.send("READY=1") }
func (n notifier) stopping() { n.send("STOPPING=1") }

// watchdog pings systemd every half interval while healthy returns true
// and ctx lives. The returned channel closes when the loop ends.
func (n notifier) watchdog(ctx context.Context, healthy func() bool) <-chan struct{} {
	done := make(chan struct{})
	if n.socket == "" || n.interval <= 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(n.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if healthy() {
					n.send("WATCHDOG=1")
				}
			}
		}
	}()
	return done
}
