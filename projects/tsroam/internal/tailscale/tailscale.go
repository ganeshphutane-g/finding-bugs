// Package tailscale drives tailscaled through its public interface, the
// tailscale CLI: "status --json" to read, "set" to change the exit node.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// CLI runs the tailscale command.
type CLI struct {
	// Bin is the tailscale binary ("tailscale": from PATH).
	Bin string
	// Socket is tailscaled's socket ("": the CLI's default).
	Socket string
	// Timeout bounds each call (default 10 s).
	Timeout time.Duration
}

func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "tailscale"
	}
	what := args[0]
	if c.Socket != "" {
		args = append([]string{"--socket=" + c.Socket}, args...)
	}
	t := c.Timeout
	if t == 0 {
		t = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("tailscale %s: %w: %s", what, err, firstLine(msg))
		}
		return nil, fmt.Errorf("tailscale %s: %w", what, err)
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// Peer is another node, as status lists it.
type Peer struct {
	ID       string
	HostName string
	DNSName  string
	IPs      []netip.Addr
	// ExitNodeOption: it offers itself as an exit node.
	ExitNodeOption bool
	Online         bool
}

// Status is what tsroam needs from "tailscale status --json".
type Status struct {
	// State is tailscaled's backend state ("Running", "Stopped",
	// "NeedsLogin", ...).
	State string
	// ExitNodeID is the exit node in use ("" none).
	ExitNodeID string
	Peers      []Peer
}

// Running says whether tailscaled is up and connected.
func (s Status) Running() bool { return s.State == "Running" }

// Find is the peer that node names, matched the way "tailscale set
// --exit-node" matches it: one of its IPs, or its DNS name, with or without
// the tailnet's domain (not its host name, which tailscale does not use).
// A name that fits several peers is an error there, and not found here.
func (s Status) Find(node string) (Peer, bool) {
	if ip, err := netip.ParseAddr(node); err == nil {
		for _, p := range s.Peers {
			for _, a := range p.IPs {
				if a == ip {
					return p, true
				}
			}
		}
		return Peer{}, false
	}
	n := strings.ToLower(strings.TrimSuffix(node, "."))
	var found []Peer
	for _, p := range s.Peers {
		dns := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
		short, _, _ := strings.Cut(dns, ".")
		if dns == n || short == n {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return Peer{}, false
	}
	return found[0], true
}

// Status reads tailscaled's status.
func (c *CLI) Status(ctx context.Context) (Status, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out)
}

func parseStatus(b []byte) (Status, error) {
	var raw struct {
		BackendState   string
		ExitNodeStatus *struct{ ID string }
		Peer           map[string]struct {
			ID             string
			HostName       string
			DNSName        string
			TailscaleIPs   []string
			ExitNodeOption bool
			Online         bool
		}
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Status{}, fmt.Errorf("reading tailscale status: %w", err)
	}
	s := Status{State: raw.BackendState}
	if raw.ExitNodeStatus != nil {
		s.ExitNodeID = raw.ExitNodeStatus.ID
	}
	for _, p := range raw.Peer {
		peer := Peer{ID: p.ID, HostName: p.HostName, DNSName: p.DNSName, ExitNodeOption: p.ExitNodeOption, Online: p.Online}
		for _, a := range p.TailscaleIPs {
			if ip, err := netip.ParseAddr(a); err == nil {
				peer.IPs = append(peer.IPs, ip)
			}
		}
		s.Peers = append(s.Peers, peer)
	}
	return s, nil
}

// SetExitNode sets the exit node ("" turns it off) and whether the local
// network stays reachable while it is on.
func (c *CLI) SetExitNode(ctx context.Context, node string, allowLAN bool) error {
	if strings.HasPrefix(node, "-") {
		return fmt.Errorf("exit node %q", node)
	}
	args := []string{"set", "--exit-node=" + node}
	if node != "" {
		args = append(args, fmt.Sprintf("--exit-node-allow-lan-access=%t", allowLAN))
	}
	_, err := c.run(ctx, args...)
	return err
}
