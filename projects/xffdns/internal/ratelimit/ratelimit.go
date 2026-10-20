// Package ratelimit limits queries per client subnet with token buckets,
// keeping a bounded number of subnets.
package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// Limiter allows rate queries a second per subnet, in bursts of up to one
// second's worth. It keeps at most about 2*max subnets: two generations,
// the older one dropped whole when the newer one fills.
type Limiter struct {
	rate      float64
	v4, v6    int
	max       int
	mu        sync.Mutex
	cur, prev map[netip.Prefix]*bucket
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New makes a Limiter. A rate of 0 allows everything.
func New(rate, v4bits, v6bits, max int) *Limiter {
	return &Limiter{rate: float64(rate), v4: v4bits, v6: v6bits, max: max,
		cur: map[netip.Prefix]*bucket{}, prev: map[netip.Prefix]*bucket{}, now: time.Now}
}

// Allow reports whether a query from a may go on, and counts it if so.
func (l *Limiter) Allow(a netip.Addr) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	a = a.Unmap()
	bits := l.v6
	if a.Is4() {
		bits = l.v4
	}
	key, err := a.WithZone("").Prefix(bits)
	if err != nil {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.cur[key]
	if b == nil {
		if b = l.prev[key]; b != nil {
			delete(l.prev, key)
		} else {
			b = &bucket{tokens: l.rate, last: now}
		}
		if len(l.cur) >= l.max {
			l.prev, l.cur = l.cur, map[netip.Prefix]*bucket{}
		}
		l.cur[key] = b
	}
	if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = min(l.rate, b.tokens+d*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Len is how many subnets are kept.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.cur) + len(l.prev)
}
