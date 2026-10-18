package check

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type pingCheck struct{ c config.Ping }

// listen opens an ICMP socket: an unprivileged one where the system allows
// it (Linux's ping sockets, macOS), else a raw one (root or CAP_NET_RAW).
func listen(v6 bool) (*icmp.PacketConn, bool, error) {
	dgram, raw, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		dgram, raw, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	if c, err := icmp.ListenPacket(dgram, addr); err == nil {
		return c, true, nil
	}
	c, err := icmp.ListenPacket(raw, addr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, false, errors.New("not allowed to send pings: run as root, give pushprobe CAP_NET_RAW, or allow ping sockets (sysctl net.ipv4.ping_group_range)")
		}
		return nil, false, err
	}
	return c, false, nil
}

func (p pingCheck) Run(ctx context.Context) Result {
	start := time.Now()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, p.c.Host)
	if err != nil || len(ips) == 0 {
		return fail(start, "%s: cannot look up the address", p.c.Host)
	}
	ip := ips[0].IP
	v6 := ip.To4() == nil
	conn, dgram, err := listen(v6)
	if err != nil {
		return fail(start, "%v", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	var dst net.Addr = &net.IPAddr{IP: ip}
	if dgram {
		dst = &net.UDPAddr{IP: ip}
	}
	var typ icmp.Type = ipv4.ICMPTypeEcho
	proto := 1
	if v6 {
		typ, proto = ipv6.ICMPTypeEchoRequest, 58
	}
	// The kernel picks the ID of a ping socket, so replies are matched by
	// sequence number and payload.
	id := rand.IntN(0xffff)
	payload := []byte(fmt.Sprintf("pushprobe %d", rand.Int64()))
	count := max(p.c.Count, 1)
	pause := 200 * time.Millisecond
	var rtts []time.Duration
	buf := make([]byte, 1500)
	for seq := 1; seq <= count; seq++ {
		msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload}}
		b, _ := msg.Marshal(nil)
		sent := time.Now()
		if _, err := conn.WriteTo(b, dst); err != nil {
			return fail(start, "%s: %s", p.c.Host, reason(err))
		}
		wait := time.Now().Add(time.Second)
		for time.Now().Before(wait) && ctx.Err() == nil {
			conn.SetReadDeadline(wait)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			reply, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil {
				continue
			}
			if echo, ok := reply.Body.(*icmp.Echo); ok && echo.Seq == seq && string(echo.Data) == string(payload) &&
				(reply.Type == ipv4.ICMPTypeEchoReply || reply.Type == ipv6.ICMPTypeEchoReply) {
				rtts = append(rtts, time.Since(sent))
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		if seq < count {
			select {
			case <-ctx.Done():
			case <-time.After(pause):
			}
		}
	}
	if len(rtts) == 0 {
		pings := "1 ping"
		if count > 1 {
			pings = fmt.Sprintf("%d pings", count)
		}
		return fail(start, "%s (%s): no reply to %s", p.c.Host, ip, pings)
	}
	var sum time.Duration
	for _, d := range rtts {
		sum += d
	}
	r := ok(start, "%s: %d/%d replies, %s on average", p.c.Host, len(rtts), count, (sum / time.Duration(len(rtts))).Round(10*time.Microsecond))
	// The time Kuma shows is the round trip, not how long the check took.
	r.Duration = sum / time.Duration(len(rtts))
	return r
}
