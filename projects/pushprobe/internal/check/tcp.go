package check

import (
	"context"
	"net"
	"time"
)

type tcpCheck struct{ address string }

func (c tcpCheck) Run(ctx context.Context) Result {
	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return fail(start, "%s", reason(err))
	}
	conn.Close()
	return ok(start, "connected")
}
