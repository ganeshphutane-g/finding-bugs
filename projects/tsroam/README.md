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
  - tailscaled's own packets;
  - loopback;
  - DHCP and IPv6 router and neighbour discovery.
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
kill switch on: only the tailnet and DHCP are reachable
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
  settled for half a second, then twice more 2 s apart);
- when Tailscale's own default route in table 52 changes, which is the exit
  node turned on or off;
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

**The kill switch's rules:**

```
table inet tsroam {
	chain out {
		type filter hook output priority filter; policy accept;
		oifname "lo" accept
		oifname "tailscale0" accept
		meta mark & 0x00ff0000 == 0x00080000 accept
		udp dport 67 accept
		udp dport 547 accept
		icmpv6 type 133-136 accept
		counter drop
	}
}
```

**`/metrics`:**
- `tsroam_network{kind}`, `tsroam_exit_node`, `tsroam_kill_switch` and
  `tsroam_paused`;
- `tsroam_changes_total` and `tsroam_problem_passes_total`;
- `tsroam_last_pass_timestamp_seconds`.

## Limits

- **Only this machine's own traffic.** The kill switch filters outgoing
  traffic; forwarded traffic (containers, VMs) is not covered.
- **Tailscale handles DNS.** With `--accept-dns=false`, a resolver on the
  café's LAN is blocked by the kill switch. Use Tailscale's DNS or a public
  resolver; both go through the exit node.
- **The exit node can only reach the internet.** Tailscale exit nodes
  forward to public addresses only.
- **IPv4 fingerprints only.** The kill switch still covers IPv6.
- **A pause is tied to a network's interface, gateway address and kind.**
  Two cafés with the same router address count as one network until
  `--for` runs out.
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
- tailscaled killed: nothing leaks; restarted: traffic is back through the
  exit node;
- `tailscale down`: nothing leaks;
- the exit node turned off by hand: tsroam turns it back on;
- `tsroam off`: traffic goes direct (the web host sees the café), and coming
  home ends the pause;
- back home: both are off;
- a `kill -9` of tsroam: the kill switch stays, and `cleanup` removes it;
- a clean stop removes it.

A mutation check showed that without the rule for tailscaled's mark, the
tests fail.

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
