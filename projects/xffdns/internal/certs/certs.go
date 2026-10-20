// Package certs serves a TLS certificate from files, reading them again
// when they change (a Let's Encrypt renewal, say).
package certs

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// Reloader holds the current certificate.
type Reloader struct {
	certFile, keyFile string
	// Every is how often the files are looked at, at most.
	Every time.Duration
	// OnReload, if set, hears of every reload after the first, and its error.
	OnReload func(error)

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string
	checked time.Time
	now     func() time.Time
}

// New reads the files once; an error here is fatal for the listener.
func New(certFile, keyFile string) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile, Every: 10 * time.Second, now: time.Now}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// stampOf names the files' current versions (size and time of change).
func (r *Reloader) stampOf() (string, error) {
	var s string
	for _, f := range []string{r.certFile, r.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		s += fmt.Sprintf("%d/%d;", fi.Size(), fi.ModTime().UnixNano())
	}
	return s, nil
}

func (r *Reloader) load() error {
	stamp, err := r.stampOf()
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.stamp = &c, stamp
	return nil
}

// GetCertificate is for tls.Config. When the files have changed it reads
// them; if they do not make a certificate (a renewal half written, say),
// the one before stays.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= r.Every {
		r.checked = now
		if stamp, err := r.stampOf(); err == nil && stamp != r.stamp {
			err := r.load()
			if err != nil {
				// Try again at the next check, not on every handshake.
				r.stamp = stamp
			}
			if r.OnReload != nil {
				r.OnReload(err)
			}
		}
	}
	return r.cert, nil
}
