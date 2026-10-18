package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"net"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type tlsCheck struct{ c config.TLS }

func (t tlsCheck) Run(ctx context.Context) Result {
	start := time.Now()
	host, _, _ := net.SplitHostPort(t.c.Address)
	name := t.c.ServerName
	if name == "" {
		name = host
	}
	d := tls.Dialer{Config: &tls.Config{ServerName: name, InsecureSkipVerify: t.c.Insecure}}
	conn, err := d.DialContext(ctx, "tcp", t.c.Address)
	if err != nil {
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		switch {
		case errors.As(err, &unknown):
			return fail(start, "the certificate is not trusted (unknown issuer)")
		case errors.As(err, &hostname):
			return fail(start, "the certificate is not for %s", name)
		case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
			return fail(start, "the certificate has expired")
		}
		return fail(start, "%s", reason(err))
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return fail(start, "no certificate")
	}
	notAfter := certs[0].NotAfter
	days := int(math.Floor(time.Until(notAfter).Hours() / 24))
	when := notAfter.UTC().Format("2006-01-02")
	switch {
	case time.Now().After(notAfter):
		return fail(start, "the certificate expired on %s", when)
	case days < *t.c.MinDaysLeft:
		return fail(start, "the certificate expires in %d days (%s); at least %d wanted", days, when, *t.c.MinDaysLeft)
	}
	return ok(start, "the certificate expires in %d days (%s)", days, when)
}
