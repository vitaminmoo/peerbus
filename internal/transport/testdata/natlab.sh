#!/usr/bin/env bash
# NAT lab for the transport package; run by TestNATLab. Needs no root:
#   unshare --user --map-root-user --net --mount testdata/natlab.sh LABNODE OUTDIR
#
#   internet (br0, 11.0.0.0/24)
#     ├── relay   11.0.0.10
#     ├── natA    11.0.0.20  masquerade ── LAN A 192.168.1.0/24: a1 .2, a2 .3
#     └── natB    11.0.0.30  masquerade ── LAN B 192.168.2.0/24: b1 .2
set -euo pipefail
BIN=$(realpath "$1")
OUT=$(realpath "$2")
mkdir -p "$OUT"
mount -t tmpfs none /run
mkdir -p /run/netns

ns() { ip netns add "$1"; ip -n "$1" link set lo up; }
run() { local n=$1; shift; ip netns exec "$n" "$@"; }

ns inet
ip -n inet link add br0 type bridge
ip -n inet link set br0 up

# plug NS into bridge BRNS:BR with address ADDR on interface IF
plug() {
	local n=$1 if=$2 addr=$3 brns=$4 br=$5
	ip link add "$if" type veth peer name "$if-p"
	ip link set "$if" netns "$n"
	ip link set "$if-p" netns "$brns"
	ip -n "$brns" link set "$if-p" master "$br" up
	ip -n "$n" addr add "$addr" dev "$if"
	ip -n "$n" link set "$if" up
}

ns relay
plug relay rw 11.0.0.10/24 inet br0

for side in A:1:20 B:2:30; do
	IFS=: read -r S N W <<<"$side"
	nat=nat$S
	ns "$nat"
	plug "$nat" wan$S 11.0.0.$W/24 inet br0
	ip -n "$nat" link add lan type bridge
	ip -n "$nat" addr add 192.168.$N.1/24 dev lan
	ip -n "$nat" link set lan up
	run "$nat" sysctl -qw net.ipv4.ip_forward=1
	run "$nat" nft add table ip nat
	run "$nat" nft "add chain ip nat post { type nat hook postrouting priority 100; }"
	run "$nat" nft add rule ip nat post oifname "wan$S" masquerade
	# Like a home router: drop unsolicited inbound to the router itself. Without
	# this, an early punch packet creates a conntrack entry that collides with the
	# host's outbound flow and masquerade picks a different port.
	run "$nat" nft add table inet filter
	run "$nat" nft "add chain inet filter input { type filter hook input priority 0; }"
	run "$nat" nft add rule inet filter input iifname "wan$S" ct state new drop
done

host() {
	local n=$1 nat=$2 addr=$3 gw=$4
	ns "$n"
	plug "$n" eth-$n "$addr/24" "$nat" lan
	ip -n "$n" route add default via "$gw"
}
host a1 natA 192.168.1.2 192.168.1.1
host a2 natA 192.168.1.3 192.168.1.1
host b1 natB 192.168.2.2 192.168.2.1

L=$OUT/local-a1
mkdir -p "$L"
S=11.0.0.10:47615
run relay "$BIN" server -listen $S >"$OUT/server.log" 2>&1 &
sleep 0.3
run a1 timeout 60 "$BIN" node -server $S -localdir "$L" -points same,lan,nat,relay,local >"$OUT/a1.log" 2>&1 &
sleep 0.5
scenario() { local name=$1 ns=$2; shift 2; run "$ns" timeout 25 "$BIN" node "$@" >"$OUT/$name.log" 2>&1 || true; }
scenario same  a1 -server $S -dial same
scenario local a1 -localdir "$L" -dial local
scenario lan   a2 -server $S -dial lan
scenario nat   b1 -server $S -dial nat
scenario relay b1 -server $S -dial relay -nopunch
kill %1 %2 2>/dev/null || true
wait 2>/dev/null || true
