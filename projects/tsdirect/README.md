# tsdirect

Keeps a Linux machine's LAN traffic on the LAN when Tailscale's subnet routes
cover that LAN, and only while the machine really is on that LAN.

With `tailscale up --accept-routes`, Linux puts the subnet routes it accepts
in table 52, behind ip rules (priorities 5210–5270) that come before the main
table. A laptop at home, on the 192.168.1.0/24 that the home subnet router
advertises, then sends its traffic for 192.168.1.x into the tailnet:
encrypted, through the subnet router and back, at the router's speed, and
only as long as the subnet router and the tailnet path to it work.

People have asked for a fix since 2021:
- [tailscale#1227](https://github.com/tailscale/tailscale/issues/1227),
  "When local route is available to a subnet, bypass tailscale subnet
  relay";
- [#7947](https://github.com/tailscale/tailscale/issues/7947), "accept
  routes at lower priority on Linux";
- [#6231](https://github.com/tailscale/tailscale/issues/6231), "accepted
  subnet routes should not have a higher priority than main table";
- plus forum threads on Tailscale, OpenWrt, NixOS and GL.iNet.

The usual workaround is a static `ip rule add to 192.168.1.0/24 lookup main`.
It is wrong as soon as the laptop leaves: a café's Wi-Fi on the same common
192.168.1.0/24 would then get traffic meant for the home LAN.

tsdirect adds that rule only while:
- the prefix is directly connected on an interface that is up and not
  Tailscale's;
- the network's fingerprint matches: the gateway's MAC address, a TCP probe
  to a host on the LAN, or both.

It removes the rule as soon as either stops being true.

```
$ sudo tsdirect learn 192.168.77.0/24
192.168.77.0/24 is on lan0; its gateway 192.168.77.1 is 02:00:5e:77:00:01
lan 192.168.77.0/24 gateway-mac 02:00:5e:77:00:01

$ sudo tsdirect run
2026-10-23T19:47:48Z tsdirect v1.0.0: 1 LAN, rules at priority 5200 marked protocol 0xd1, looking every 15s and on network changes
2026-10-23T19:47:48Z added rule to 192.168.77.0/24 lookup main priority 5200: on 192.168.77.0/24 via lan0 (gateway 192.168.77.1 is 02:00:5e:77:00:01)
    ... the laptop goes to a café whose Wi-Fi is also 192.168.77.0/24 ...
2026-10-23T19:47:56Z removed rule to 192.168.77.0/24 lookup main priority 5200: on a 192.168.77.0/24 network on lan0, but its gateway 192.168.77.1 is 02:00:5e:77:01:01, not 02:00:5e:77:00:01: another network
    ... and comes home ...
2026-10-23T19:47:56Z added rule to 192.168.77.0/24 lookup main priority 5200: on 192.168.77.0/24 via lan0 (gateway 192.168.77.1 is 02:00:5e:77:00:01)
```

These are real runs, from the end-to-end tests: headscale and tailscaled in
network namespaces, with the client's cable moved between two LANs. The
version and interval are shown as they would be in an installed run.

## Install

Linux 4.17 or newer (older kernels drop a rule's protocol value, which is
how tsdirect finds its own rules; it refuses to run on them). It needs root,
or CAP_NET_ADMIN (ip rules) and CAP_NET_RAW (ARP requests and probes bound
to an interface).

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/tsdirect
make build                      # ./tsdirect, a static binary
sudo install -m 755 tsdirect /usr/local/bin/
sudo install -m 644 contrib/tsdirect.service /etc/systemd/system/
```

The unit runs it as a dynamic user with only those two capabilities, after
tailscaled.

## Quickstart

On the LAN, find its fingerprint and put it in the config:

```
sudo tsdirect learn 192.168.1.0/24 | sudo tee -a /etc/tsdirect.conf
```

Check what tsdirect sees (it changes nothing):

```
$ sudo tsdirect check
LAN              INTERFACE  STATE   RULE  SOURCE
192.168.77.0/24  lan0       direct  no    /etc/tsdirect.conf:1

192.168.77.0/24: on 192.168.77.0/24 via lan0 (gateway 192.168.77.1 is 02:00:5e:77:00:01) [run would add the rule]
```

Then start it:

```
sudo systemctl enable --now tsdirect
```

Check `ip route get 192.168.1.10`: on the LAN it says `dev wlan0` (or your
LAN interface), not `dev tailscale0 table 52`.

## Configuration

`/etc/tsdirect.conf` has one `lan` line per network; `#` starts a comment.

```
# Home: the router's MAC.
lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56

# Office: no stable gateway MAC (two routers in failover), so a probe to the
# file server's SSH port.
lan 10.10.0.0/16 probe 10.10.0.5:22

# Both, and the MAC of a given address (a LAN without a default route on
# that interface).
lan 192.168.50.0/24 gateway 192.168.50.254 gateway-mac 00:11:22:33:44:55 probe 192.168.50.10:443
```

- `gateway-mac MAC`: the main table's default gateway on the LAN's interface
  must have this MAC. tsdirect sends an ARP request each time it looks; it
  does not trust the neighbour cache, which can still hold the last
  network's entry.
- `gateway ADDR`: check this address's MAC instead of the default gateway's.
- `probe HOST:PORT`: a TCP connection to it must succeed. The connection is
  bound to the LAN interface (SO_BINDTODEVICE), so it cannot reach the host
  through the tailnet. Without that, a probe from the café would succeed
  through the subnet router; the end-to-end tests check this. A refused
  connection counts as a failure, so pick a port that is open.
- A LAN must have a fingerprint. LANs must not overlap. IPv4 only, for
  now.

LANs can also be given with `--lan "192.168.1.0/24 gateway-mac ..."`
(repeat), and `--config ""` reads no file. `systemctl reload tsdirect`
(SIGHUP) reads the file again.

## Commands

| Command | What it does |
|---|---|
| `tsdirect run` | Keeps the rules right until stopped, then removes them. |
| `tsdirect check` | One look: each LAN, its state, whether its rule is there, and why. Changes nothing. |
| `tsdirect learn PREFIX` | Prints the `lan` line for the network the machine is on now (`--gateway ADDR`, `--probe HOST:PORT`). |
| `tsdirect cleanup` | Removes every rule tsdirect added (after a `kill -9`, or before uninstalling). |
| `tsdirect version` | Prints the version. |

Flags for `run` and `check`:

| Flag | Default | |
|---|---|---|
| `--config` | `/etc/tsdirect.conf` | A missing file is no LANs. |
| `--lan` | | A LAN, as in the config (repeat). |
| `--priority` | 5200 | The rules' priority; it must come before Tailscale's 5210 to win. |
| `--rule-proto` | 0xd1 | The protocol value that marks tsdirect's rules (5–255). |
| `--interval` | 15s | (`run`) How often to look, besides on network changes. |
| `--listen` | off | (`run`) Serve `/metrics` and `/healthz`, e.g. `127.0.0.1:9587`. |

`check` exit codes: 0 fine; 3 the machine is on a LAN's addresses but the
fingerprint does not match (or the LAN is only partly there); 1 error; 2 bad
usage.

```
$ sudo tsdirect check   # at the café
LAN              INTERFACE  STATE          RULE  SOURCE
192.168.77.0/24  lan0       not confirmed  no    /etc/tsdirect.conf:1

192.168.77.0/24: on a 192.168.77.0/24 network on lan0, but its gateway 192.168.77.1 is 02:00:5e:77:01:01, not 02:00:5e:77:00:01: another network
$ echo $?
3
```

## How it works

For each LAN, tsdirect looks:
1. at the main table, for a connected route covering the whole prefix, on
   an interface that is up and not `tailscale*`;
2. at the fingerprint, step by step. No probe is sent onto a network whose
   gateway MAC is already wrong.

Then it adds or removes `ip rule to PREFIX lookup main priority 5200 proto
0xd1`. The rule only sends that prefix to the main table, where the
connected route is.

It looks on every address, route and link change (netlink events, settled
for half a second, then twice more 2 s apart, for a gateway that is slow to
answer on a network just joined) and every `--interval`. If the kernel drops
events, tsdirect removes its rules and exits with an error, so systemd
restarts it, rather than miss a move to another network. Tailscale's rules are its own and
tsdirect never touches them. tailscaled restarts delete and re-add only
Tailscale's rules, so tsdirect's rule stays.

Its rules are found by their protocol value, so tsdirect only ever removes
its own:
- on exit, and when a start fails (a broken config): a crashed run's rule
  does not outlive it;
- at start, after a crash: the rule is kept, not added twice;
- when the priority or the configured LANs change;
- with `cleanup`.

`/metrics` has, per LAN, `tsdirect_lan_connected`, `tsdirect_lan_direct`,
`tsdirect_lan_rule` and `tsdirect_rule_changes_total`; it also has
`tsdirect_errors_total` and `tsdirect_last_pass_timestamp_seconds`.

## Limits

- **A fingerprint is not authentication.** It tells your LAN from another
  network that happens to use the same addresses. It does not stop a hostile
  network that copies your gateway's MAC, or answers your probe port. Traffic
  that needs protection on a strange network needs encryption of its own.
  Without tsdirect, the static-rule workaround fails worse: it trusts any
  network with the addresses.
- **IPv4 only.** IPv6 LANs are refused in the config: the test environment
  had no IPv6, and an untested fingerprint path is one that could fail
  open.
- **Linux only.** macOS and Windows route differently.
- **Exit nodes are out of scope.** `--exit-node-allow-lan-access` already
  exists for them.

## Testing

`make test` runs the unit tests (CI also runs them on big-endian MIPS under
qemu). `make test-tailscale` (root) also runs the
end-to-end tests. These start headscale and two real tailscaled (a subnet
router and a client with `--accept-routes`) in network namespaces, on two
LANs with the same addresses. They check:
- the problem itself: LAN traffic goes to `tailscale0` before tsdirect runs;
- (tsdirect runs with `--interval 10m` throughout, so every reaction below
  comes from network events);
- at home: the rule is added, traffic goes direct, and still works with the
  subnet router down;
- restarting tailscaled: the rule stays;
- at the café: no rule;
- leaving the LAN (address gone, link down): the rule goes;
- a `kill -9`: one rule after restarting, and `cleanup` removes it;
- a probe fingerprint: at the café, where the NAS is reachable through the
  tailnet, the probe does not count it.

They need tailscale, tailscaled and headscale on PATH:

```
GOTOOLCHAIN=auto go install tailscale.com/cmd/tailscale@v1.104.2 tailscale.com/cmd/tailscaled@v1.104.2
go install github.com/juanfont/headscale/cmd/headscale@v0.29.5
```

CI runs both suites on every change.

## Roadmap

- Use Tailscale's own setting for this if one lands: an open upstream PR
  ([#21591](https://github.com/tailscale/tailscale/pull/21591)) adds static
  allow and deny lists for accepted routes. tsdirect could then drive the
  deny list instead of ip rules.
- IPv6 LANs (NDP for the gateway MAC), tested end to end.
- A fingerprint from the Wi-Fi network (BSSID or SSID through
  NetworkManager) as a third option.
- The same detection for the reverse case: use an exit node automatically
  on networks that are not one of yours.

## License

MIT, see [LICENSE](LICENSE).
