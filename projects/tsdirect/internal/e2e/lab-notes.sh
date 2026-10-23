#!/bin/bash
# Lab: inet (central router) -- hs (headscale 10.50.0.2)
#                            -- gw (home router, WAN 10.51.0.2, LAN br 192.168.77.1/24, NAT)
#      LAN: sr (subnet router 192.168.77.10), nas (192.168.77.20), cl (client 192.168.77.30)
set -e
L=$1; B=/tmp/ts-bin
for ns in inet hs gw sr nas cl; do ip netns add t$ns; ip -n t$ns link set lo up; done
link() { ip link add $1 type veth peer name $2; ip link set $1 netns $3; ip link set $2 netns $4; }
link ih0 hs0 tinet ths; link ig0 gw0 tinet tgw
ip -n tinet addr add 10.50.0.1/24 dev ih0; ip -n ths addr add 10.50.0.2/24 dev hs0
ip -n tinet addr add 10.51.0.1/24 dev ig0; ip -n tgw addr add 10.51.0.2/24 dev gw0
for x in "tinet ih0" "tinet ig0" "ths hs0" "tgw gw0"; do set -- $x; ip -n $1 link set $2 up; done
ip netns exec tinet sysctl -qw net.ipv4.ip_forward=1
ip -n ths route add default via 10.50.0.1
ip -n tgw route add default via 10.51.0.1
# LAN bridge in gw
ip -n tgw link add br0 type bridge; ip -n tgw addr add 192.168.77.1/24 dev br0; ip -n tgw link set br0 up
ip netns exec tgw sysctl -qw net.ipv4.ip_forward=1
ip netns exec tgw nft add table ip nat
ip netns exec tgw nft add chain ip nat post '{ type nat hook postrouting priority 100 ; }'
ip netns exec tgw nft add rule ip nat post oifname gw0 masquerade
n=10
for h in sr nas cl; do
  ip link add l$h type veth peer name b$h; ip link set l$h netns t$h; ip link set b$h netns tgw
  ip -n tgw link set b$h master br0; ip -n tgw link set b$h up
  ip -n t$h link set l$h up
done
ip -n tsr addr add 192.168.77.10/24 dev lsr; ip -n tnas addr add 192.168.77.20/24 dev lnas; ip -n tcl addr add 192.168.77.30/24 dev lcl
for h in sr nas cl; do ip -n t$h route add default via 192.168.77.1; done
ip netns exec tsr sysctl -qw net.ipv4.ip_forward=1
# headscale
mkdir -p $L/hs
sed -e "s#^server_url:.*#server_url: http://10.50.0.2:8080#" -e "s#^listen_addr:.*#listen_addr: 0.0.0.0:8080#" \
    -e "s#/var/lib/headscale#$L/hs#g" -e "s#/var/run/headscale#$L/hs#g" \
    -e "s#^  magic_dns: true#  magic_dns: false#" -e "s#^  override_local_dns: true#  override_local_dns: false#" \
    -e "s#^  auto_update_enabled: true#  auto_update_enabled: false#" \
    /root/go/pkg/mod/github.com/juanfont/headscale@v0.29.5/config-example.yaml > $L/hs/config.yaml
python3 - $L/hs/config.yaml <<'PY'
import sys,re
p=sys.argv[1]; s=open(p).read()
s=s.replace("  urls:\n    - https://controlplane.tailscale.com/derpmap/default\n","  urls: []\n")
s=re.sub(r"(  server:\n    enabled: )false", r"\1true", s)
s=s.replace("    ipv4: 198.51.100.1\n","    ipv4: 10.50.0.2\n").replace("    ipv6: 2001:db8::1\n","")
s=re.sub(r"  nameservers:\n    global:\n(      - .*\n)+","  nameservers:\n    global: []\n",s)
open(p,'w').write(s)
PY
(ip netns exec ths $B/headscale -c $L/hs/config.yaml serve > $L/hs/log 2>&1 &)
sleep 3
