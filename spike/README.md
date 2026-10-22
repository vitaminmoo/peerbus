# Transport spikes

Two prototypes of the peer-to-peer transport, compared in the same NAT lab.

- `p2p/`: go-libp2p (QUIC, Noise, circuit relay v2, DCUtR hole punching).
- `slim/`: hand-assembled. QUIC (quic-go) with TLS 1.3 and pinned Ed25519
  certs, one UDP socket per node shared with our own control packets
  (RFC 9443 multiplexing), and a small server that does STUN-style address
  discovery, signaling, and TURN-style forwarding, all only between
  registered, unguessable ids.

## Lab

`lab.sh` in each directory builds this inside an unprivileged user namespace
(no root needed):

```
internet 11.0.0.0/24 ── relay 11.0.0.10
                     ├─ natA 11.0.0.20 (masquerade + drop unsolicited) ── a1 192.168.1.2, a2 .3
                     └─ natB 11.0.0.30 (masquerade + drop unsolicited) ── b1 192.168.2.2
```

```sh
cd slim && go build -o slimspike . && unshare --user --map-root-user --net --mount ./lab.sh ./slimspike /tmp/slimlab
cd p2p  && go build -o p2pspike  . && unshare --user --map-root-user --net --mount ./lab.sh ./p2pspike  /tmp/p2plab
```

Two lab gotchas, both of which first looked like transport bugs:
- libp2p doesn't treat documentation ranges (203.0.113.0/24) as public, so
  hole punching never started. The lab uses 11.0.0.0/24 instead.
- A NAT box without an input firewall answers early punch packets itself. The
  resulting conntrack entry collides with the host's outbound flow and
  masquerade remaps the port, so punching fails for both implementations.
  Real home routers drop unsolicited input; the lab does too.

## Results (2026-10-21)

| | libp2p | slim |
|---|---|---|
| Modules in graph / linked | 136 / many | 16 / 4 (quic-go, x/crypto, x/net, x/sys) |
| Binary | 34 MB | 10 MB |
| Spike code | 283 lines | 777 lines |
| Same machine | (not run) | direct 127.0.0.1, 2.2 ms to connect |
| Same LAN | direct, 34 ms | direct, 1.9 ms |
| Different NATs | relayed at 1.0 s, direct by 1.2 s | direct at 2.0 ms |
| Relay only | works; default limits of 2 min / 128 KiB per relayed conn need overriding | works, 1.8 ms; no limits yet |
| Unregistered id via relay | refused (`NO_RESERVATION`) | refused (`not registered`) |

Not covered by either lab: symmetric NATs/CGNAT, IPv6, UDP-blocked networks
(slim has no TCP fallback yet), real devices on real networks.
