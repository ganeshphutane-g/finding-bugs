# tsroam

Uses a Tailscale exit node automatically on Linux whenever the machine is
not on one of its own networks. An optional kill switch makes sure nothing
leaves outside the tailnet while it should not, even when tailscaled
crashes, restarts or is brought down.

Tailscale has no such policy on Linux. On café or hotel Wi-Fi a laptop uses
whatever exit-node setting was last left. If tailscaled dies, its routes go
with it, and traffic goes straight out on the café's network. (The lab below
shows both.) People have asked for this since 2021:
- [tailscale#3302](https://github.com/tailscale/tailscale/issues/3302),
  "Automatically use an Exit Node on Untrusted/New Wifi Networks", with 127
  👍;
- requests for a kill switch while tailscaled updates
  ([#10773](https://github.com/tailscale/tailscale/issues/10773));
- requests for an exit node on demand
  ([#13992](https://github.com/tailscale/tailscale/issues/13992)).

The existing tools are for Windows or macOS only, or are manual pickers.

tsroam decides which network the machine is on: the one its default route
leaves on.
- **Trusted** means a network listed in the config whose addresses and
  fingerprint match. The fingerprint is the gateway's MAC address (asked
  fresh with ARP), a TCP probe made from that interface, or both. A café
  using the same 192.168.1.0/24 as home is therefore not taken for home.
- **Anywhere else is untrusted.** There tsroam runs `tailscale set
  --exit-node=...`. With `kill-switch on`, it also installs an nftables table
  that lets out only:
  - tailnet traffic;
  - tailscaled's own packets, and tsroam's own trust probes;
  - loopback;
  - a DHCP client's requests;
  - IPv6 router and neighbour discovery on the link.

  This applies to the machine's own traffic and to traffic it forwards
  (containers, VMs).
- **Back on a trusted network**, it turns the exit node off and removes the
  table.

```
$ sudo tsroam learn
192.168.77.0/24 is on lan0; its gateway 192.168.77.1 is 02:00:5e:77:00:01
trusted 192.168.77.0/24 gateway-mac 02:00:5e:77:00:01

$ sudo tsroam run
tsroam v1.0.0: exit node sr except on 1 trusted network, kill switch on, looking every 15s and on network changes
trusted: the trusted network 192.168.77.0/24 (/etc/tsroam.conf:3)
    ... the laptop joins a café's Wi-Fi, also 192.168.77.0/24, another router ...
kill switch on: the network changed; looking at it
kill switch updated: only the tailnet and DHCP are reachable
exit node on: sr
untrusted: the network on lan0 is not a trusted one
    ... tailscaled is killed: nothing leaves; it restarts ...
note: tailscaled is in state Starting; the exit node is set when it runs
    ... someone turns the exit node off by hand ...
exit node on: sr
    ... back home ...
kill switch off
exit node off
trusted: the trusted network 192.168.77.0/24 (/etc/tsroam.conf:3)
```

These are real runs from the end-to-end tests: headscale and tailscaled in
network namespaces, with the client's cable moved between a home LAN and a
café LAN that use the same addresses. The timestamps are left out, the
version is shown as an installed run prints it, and the config path is
shown as the default.

## Install

Linux, with Tailscale's tailscaled. With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/tsroam
make build                      # ./tsroam, a static binary
sudo install -m 755 tsroam /usr/local/bin/
sudo install -m 644 contrib/tsroam.service /etc/systemd/system/
```

tsroam runs as root, because `tailscale set` needs root (or the operator
user). Its unit keeps only CAP_NET_ADMIN (nftables) and CAP_NET_RAW (ARP
requests and bound probes). The lab checked that those two are enough.

## Quickstart

1. Choose the exit node: a node that offers itself as one
   (`tailscale up --advertise-exit-node`, then approved). `tailscale status`
   lists them as "offers exit node".
2. On each network you trust, add its line:

   ```
   sudo tsroam learn | sudo tee -a /etc/tsroam.conf
   ```
3. Add the exit node, and the kill switch if you want it:

   ```
   exit-node sr
   kill-switch on
   trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
   ```
4. Check, then start:

   ```
   $ sudo tsroam check
   network:     trusted (default route on lan0 via 192.168.77.1)
                the trusted network 192.168.77.0/24 (/etc/tsroam.conf:3)
   trusted:     192.168.77.0/24 (/etc/tsroam.conf:3): on 192.168.77.0/24 via lan0 (gateway 192.168.77.1 is 02:00:5e:77:00:01)
   exit node:   not sr
   kill switch: off
   run would:   exit node off on arrival, kill switch off

   $ sudo systemctl enable --now tsroam
   ```

On an untrusted network, `check` exits 4 and says what `run` does there:

```
network:     untrusted (default route on lan0 via 192.168.77.1)
             the network on lan0 is not a trusted one
exit node:   not sr
kill switch: off
run would:   exit node sr on, kill switch on
```

## Captive portals

Hotel and café Wi-Fi often wants a login page first, which neither the exit
node nor the kill switch lets through. Run:

```
$ sudo tsroam off
kill switch and exit node off on this network (untrusted), until it changes or 19:35
```

Both stay off until the machine moves to another network, or until `--for`
runs out (10 minutes by default). Then tsroam takes over again.

## Configuration

`/etc/tsroam.conf`; `#` starts a comment.

| Line | |
|---|---|
| `exit-node NODE` | The exit node, as `tailscale set --exit-node` takes it: a Tailscale IP or a node name. Required. |
| `kill-switch on\|off` | Off by default. |
| `allow-lan on\|off` | Off by default. When on, the local network stays reachable on untrusted networks (Tailscale's `--exit-node-allow-lan-access`, and the kill switch lets the LAN's prefix through). |
| `trusted PREFIX [gateway-mac MAC] [gateway ADDR] [probe ADDR:PORT]` | A trusted network. It needs a fingerprint: `gateway-mac`, `probe`, or both. `gateway` checks that address's MAC instead of the default gateway's. |

The fingerprint rules are tsdirect's and fail closed:
- **Connected:** the prefix must be directly connected on the interface the
  default route uses, and no other route may take part of it.
- **ARP, not the cache:** the gateway's MAC is asked with a fresh ARP
  request. The neighbour cache can still hold the last network's entry.
- **Bound probe:** a probe is a TCP connection bound to that interface. It
  cannot be answered through the tailnet, and it never counts the machine's
  own address.
- **Unknown is untrusted:** anything tsroam cannot check counts as
  untrusted, so the exit node stays on.

## Commands

| Command | What it does |
|---|---|
| `tsroam run` | Applies the policy until stopped. Removes the kill switch on a clean stop; a crash leaves it in place. |
| `tsroam check` | One look: the network, each trusted line's verdict, the exit node, the kill switch, and what run would do. Changes nothing. Exit 0: trusted or offline; 4: untrusted; 1: a problem. |
| `tsroam learn [PREFIX]` | Prints the `trusted` line for the network the machine is on (`--gateway`, `--probe`). |
| `tsroam off [--for 10m]` | Captive portals: turns both off until the network changes or the time runs out. |
| `tsroam cleanup` | Removes the kill switch and any pause, for example after a crash or before uninstalling. |

Flags (`tsroam run -h` lists them all):

| Flag | Default | |
|---|---|---|
| `--config` | `/etc/tsroam.conf` | |
| `--exit-node` | the config's | |
| `--tailscale` | `tailscale` | The tailscale command. |
| `--socket` | the command's default | tailscaled's socket. |
| `--tun` | `tailscale0` | Tailscale's interface. |
| `--ts-mark` | `0x80000/0xff0000` | The mark tailscaled puts on its own packets. |
| `--state-dir` | `/run/tsroam` | Where `off` keeps its pause. |
| `--interval` | 15s | (`run`) How often to look, besides on events. |
| `--listen` | off | (`run`) `/metrics` and `/healthz`. |

## How it works

**When it looks:**
- on every address, link and main-table route change (netlink events,
  settled for half a second, then twice more 2 s apart). With the kill
  switch configured, a change that moves the default route puts the switch
  in at once, before that look. It also goes in while the machine is
  offline. A trusted verdict takes it out;
- when Tailscale's own default route in table 52 changes, which is the exit
  node turned on or off;
- every 2 s, a check that the kill switch is still in place with all its
  rules (`systemctl restart nftables` flushes the ruleset), and whether
  `tsroam off` has paused it;
- every `--interval`.

**On an untrusted network:**
1. The kill switch goes in first. It is one nftables transaction that
   replaces tsroam's own `inet tsroam` table and no other.
2. Then the exit node is set.
3. Both are enforced on every pass. Someone turning the exit node off is
   undone, and a removed table comes back.

**On a trusted network:** the exit node is turned off once, on arrival, as
soon as tailscaled runs. After that you may choose an exit node at home, and
tsroam leaves it alone.

**Offline:** nothing changes.

**tailscaled down:** if tailscaled is not running, the kill switch holds and
the exit node is set once it is back. The lab measured about a second from
restart to working traffic.

**The kill switch's rules** (on a network whose interface is wlan0):

```
table inet tsroam {
	chain out {
		type filter hook postrouting priority filter; policy drop;
		oifname "lo" accept
		oifname "tailscale0" accept
		meta mark & 0x00ff0000 == 0x00080000 accept
		meta mark 0x01000000/8 accept
		oifname "wlan0" udp sport 68 udp dport 67 accept
		oifname "wlan0" udp sport 546 udp dport 547 accept
		icmpv6 type 133-136 ip6 daddr fe80::/10 accept
		icmpv6 type 133-136 ip6 daddr ff02::/16 accept
		counter packets 0 bytes 0 drop
	}
}
```

- **Postrouting:** the chain hooks postrouting, so forwarded traffic is
  covered, and its policy is drop, so a flushed chain still lets nothing
  out.
- **Marks:** `0x80000` is tailscaled's mark. `0x01000000` marks tsroam's own
  trust probes, so a trusted network can be recognised with the switch on.
- **DHCP:** only from the client ports, which are privileged, so no ordinary
  program can use them to get around the switch.
- **allow-lan:** with it on, a rule for the LAN's prefix is added, but only
  for a private or link-local network of /8 or narrower. A hostile DHCP
  netmask cannot make half the internet "the LAN".

**`/metrics`:**
- `tsroam_network{kind}`, `tsroam_exit_node`, `tsroam_kill_switch` and
  `tsroam_paused`;
- `tsroam_changes_total` and `tsroam_problem_passes_total`;
- `tsroam_last_pass_timestamp_seconds`.

## Limits

- **Tailscale handles DNS.** With `--accept-dns=false`, a resolver on the
  café's LAN is blocked by the kill switch. Use Tailscale's DNS or a public
  resolver; both go through the exit node.
- **The exit node can only reach the internet.** Tailscale exit nodes
  forward to public addresses only.
- **IPv4 fingerprints only.** A network with only an IPv6 default route is
  untrusted. The kill switch covers IPv6 too.
- **How a pause tells networks apart:** by interface, gateway address,
  prefix and the gateway's MAC (asked fresh), and it ends when the machine
  goes offline. A network that copies all of those keeps the pause until
  `--for` runs out.
- **The LAN-access setting:** Tailscale's `--exit-node-allow-lan-access` is
  set on each arrival on an untrusted network. A manual change while there
  is kept until the next move.
- **Exit nodes by name:** they are matched the way `tailscale set
  --exit-node` matches them: an IP, or a DNS name with or without the
  tailnet's domain. A name that fits two nodes is refused.
- **Linux only.**

## Testing

`make test` runs the unit tests. The kill-switch tests run in real network
namespaces when run as root. CI also runs the unit tests on big-endian MIPS
under qemu.

`make test-tailscale` (root) runs the end-to-end tests. They start headscale,
an exit node, a client, a home LAN, a café LAN with the same addresses, and a
public web host that reports which address it sees. tsroam runs with
`--interval 10m`, so events drive every reaction. They check:
- at home: no exit node, no kill switch;
- at the café: the exit node is on (the web host sees the home gateway's
  address), the kill switch is in place, and the café's own host cannot be
  reached;
- tailscaled killed: nothing leaks, from the machine or from a container
  behind it, and the café's host cannot be reached; restarted: traffic is
  back through the exit node;
- `tailscale down`: nothing leaks;
- the exit node turned off by hand: tsroam turns it back on;
- `tsroam off`: traffic goes direct (the web host sees the café), and coming
  home ends the pause;
- back home: both are off. Home is recognised by its router's MAC and a
  probe, and the probe works with the kill switch still on;
- a `kill -9` of tsroam: the kill switch stays, and `cleanup` removes it;
- a clean stop removes it.

Mutation checks showed that the tests fail if any of these is taken out:
the rule for tailscaled's mark, the probe mark, or the postrouting hook
(which covers forwarded traffic).

The end-to-end tests need tailscale, tailscaled and headscale on PATH:

```
GOTOOLCHAIN=auto go install tailscale.com/cmd/tailscale@v1.104.2 tailscale.com/cmd/tailscaled@v1.104.2
go install github.com/juanfont/headscale/cmd/headscale@v0.29.5
```

## Roadmap

- Wi-Fi SSIDs (NetworkManager) as a fingerprint.
- Bypassing the exit node for chosen addresses or domains
  ([#7766](https://github.com/tailscale/tailscale/issues/7766), 50 👍).
- A forward-hook option for containers and VMs.
- Several exit nodes, with failover.

## License

MIT, see [LICENSE](LICENSE).
