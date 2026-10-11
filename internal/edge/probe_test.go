package edge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func TestCertificateReadinessRequiresAValidTrustedChain(t *testing.T) {
	now := time.Now().UTC()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial := int64(0)
	issue := func(template, parent *x509.Certificate) *x509.Certificate {
		t.Helper()
		serial++
		template.SerialNumber = big.NewInt(serial)
		if parent == nil {
			parent = template
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	root := issue(&x509.Certificate{Subject: pkix.Name{CommonName: "Test Root"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, nil)
	intermediate := issue(&x509.Certificate{Subject: pkix.Name{CommonName: "Test Intermediate"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, root)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	leaf := func(host string, before, after time.Time) *x509.Certificate {
		return issue(&x509.Certificate{Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: before, NotAfter: after}, intermediate)
	}
	valid := leaf("site.example.com", now.Add(-time.Minute), now.Add(time.Minute))
	cases := []struct {
		name  string
		certs []*x509.Certificate
		roots *x509.CertPool
		want  bool
	}{
		{"trusted with intermediate", []*x509.Certificate{valid, intermediate}, roots, true},
		{"untrusted issuer", []*x509.Certificate{valid, intermediate}, x509.NewCertPool(), false},
		{"missing intermediate", []*x509.Certificate{valid}, roots, false},
		{"not valid yet", []*x509.Certificate{leaf("site.example.com", now.Add(time.Minute), now.Add(time.Hour)), intermediate}, roots, false},
		{"expired", []*x509.Certificate{leaf("site.example.com", now.Add(-time.Hour), now.Add(-time.Minute)), intermediate}, roots, false},
		{"other host", []*x509.Certificate{leaf("other.example.com", now.Add(-time.Minute), now.Add(time.Minute)), intermediate}, roots, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, why, expiry, found := certificateReadiness("site.example.com", tt.certs, now, tt.roots)
			if got != tt.want || !found || expiry.IsZero() || why == "" {
				t.Fatalf("ready=%t, why=%q, expiry=%s, found=%t", got, why, expiry, found)
			}
		})
	}
}
