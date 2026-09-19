package app

import (
	"context"
	"net"
	"time"
)

// net_Dialer wraps net.Dialer with the short timeout readiness checks want.
type net_Dialer struct{}

func (net_Dialer) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}
