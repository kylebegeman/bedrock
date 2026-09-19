package edge

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Resolves says whether host's DNS points at one of this machine's
// addresses, and what it points at when it doesn't.
func Resolves(ctx context.Context, host string, machineAddrs []string) (ok bool, pointsAt []string, err error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, nil, fmt.Errorf("%s doesn't resolve: %w", host, err)
	}
	mine := map[string]bool{}
	for _, a := range machineAddrs {
		mine[a] = true
	}
	for _, ip := range ips {
		pointsAt = append(pointsAt, ip.IP.String())
		if mine[ip.IP.String()] {
			ok = true
		}
	}
	sort.Strings(pointsAt)
	return ok, pointsAt, nil
}

// DNSProblem explains a host that doesn't point here, in plain words.
func DNSProblem(host string, pointsAt, machineAddrs []string) string {
	return fmt.Sprintf("%s points at %s; this machine is %s", host, strings.Join(pointsAt, " and "), strings.Join(machineAddrs, " and "))
}

// CertificateReady reports whether the edge at addr serves a real
// certificate for host: one that names the host and isn't Caddy's own
// local authority's.
func CertificateReady(ctx context.Context, host, addr string) (bool, string) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: host, InsecureSkipVerify: true}, //nolint:gosec // we inspect the certificate ourselves
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err.Error()
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return false, "no certificate"
	}
	leaf := certs[0]
	if strings.Contains(leaf.Issuer.CommonName, "Caddy Local Authority") {
		return false, "only Caddy's local certificate so far"
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return false, "the certificate is for " + strings.Join(leaf.DNSNames, ", ")
	}
	if time.Now().After(leaf.NotAfter) {
		return false, "the certificate expired " + leaf.NotAfter.Format(time.DateOnly)
	}
	return true, "issued by " + leaf.Issuer.CommonName
}

// WaitCertificate polls until the edge serves a real certificate for
// host, or the wait runs out.
func WaitCertificate(ctx context.Context, host, addr string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	var last string
	for {
		ok, why := CertificateReady(ctx, host, addr)
		if ok {
			return why, nil
		}
		last = why
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no certificate for %s after %s: %s", host, limit, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
