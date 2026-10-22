# peerbus

Message passing between Claude Code sessions across Claude accounts,
machines, and friends you've paired with. Setup guide: https://peerb.us

Claude Code's built-in cross-session messaging covers sessions under one
account on one machine. peerbus covers the rest. Messages land the same
way: between tool calls in a busy session, as a new turn in an idle one,
with a one-line preview in the terminal and a thread to reply on.

## Status

Early, and partway through a change of design.

| Piece | State |
|---|---|
| Channel server and its tools | Working, on the original broker design |
| Messaging between sessions on one machine, any Claude account | Working |
| P2P transport: QUIC, hole punching, relay, STUN | Working and tested, not yet used by the channel |
| Identity, invites and pairing (`peerbus invite` / `pair` / `peer`) | Working, not yet used by the channel |
| Rendezvous server (`peerbus rendezvous`) | Working; public instance at `rv.peerb.us:47615` |
| Channel on the P2P transport: other machines and friends | Next |

Until the channel moves over, the original broker (below) is how sessions
actually talk.

## How it works

```
Claude session ─stdio─ peerbus channel ══ QUIC ══ peerbus channel ─stdio─ Claude session
                              └──── rendezvous server: introductions, relay ────┘
```

- Each session runs `peerbus channel`, an MCP server that declares Claude
  Code's [`claude/channel`](https://code.claude.com/docs/en/channels-reference)
  capability, so incoming messages are pushed into the session.
- Sessions connect to each other over QUIC (TLS 1.3) with each side pinned to
  the other's Ed25519 key. One UDP socket per session carries both QUIC and
  peerbus's control packets (RFC 9443 multiplexing).
- Paths, most direct first: loopback on the same machine, LAN addresses,
  hole punching across NATs, then the relay. A STUN query finds the real
  public address when the rendezvous server shares your NAT.
- The rendezvous server lists sessions only to someone who names a meeting
  point. Meeting points are HMACs of a secret held by the two paired sides
  and rotate hourly. The server can't enumerate anyone, and it only forwards
  to sessions that are currently registered.
- Everyone has one identity key, shared across their own machines. Friends
  pin that key once.
- Invites are single-use codes holding the inviter's key and a one-time
  secret. The redeemer proves the secret with an HMAC over the TLS exporter,
  so even the rendezvous operator can't use an invite they see waiting.
- Each peer has permissions, enforced by the receiving side: hold or
  auto-deliver their messages, which of your sessions they can address, and
  whether they see your working directories and advertised resources.

## Install

Go 1.27 or newer. Linux; macOS should work but is untested.

```sh
go install github.com/vitaminmoo/peerbus/cmd/peerbus@latest
```

Register the channel with each Claude account you use:

```sh
claude mcp add -s user peerbus -- peerbus channel
CLAUDE_CONFIG_DIR=~/.claude-work claude mcp add -s user peerbus -- peerbus channel
```

Start sessions with the channel enabled. Channels are a research preview,
and custom channels need this flag, which asks for confirmation on every
launch:

```sh
claude --dangerously-load-development-channels server:peerbus
```

Servers loaded with `--mcp-config` don't count for `server:` channel entries.

## Using it today: the broker

```sh
peerbus init      # broker + client config in ~/.config/peerbus
```

The broker starts on demand and keeps running in the background (log and pid
in `~/.local/state/peerbus`). Every Claude account on the machine shares the
client config, so they all act as one user and message each other freely.

The broker can also run on a server for other machines and other users,
with tokens, groups, and human approval for messages between users. See
`peerbus help`. That setup is superseded by pairing and will go away when the
channel moves to the P2P transport.

Tools Claude gets:

| Tool | |
|---|---|
| `list_peers` | Reachable sessions, their status and host, and what they advertise (MCP servers, programs, repos). Optional filter. |
| `send` | Plain text to a session; pass `thread` to continue a conversation. |
| `subscribe_idle` | One notice when a session next finishes its turn or disconnects. |
| `advertise` | Publish extra resources this session can offer. |

## Pairing

These work now and are what the channel will use after the switch.

```sh
peerbus id -name yourname     # your fingerprint and the name you suggest
peerbus invite -self          # add one of your own machines
peerbus invite                # invite a friend; send them the code privately
peerbus pair pb1-…            # redeem a code
peerbus peer list
peerbus peer set alice deliver=auto sessions='ghidra-*'
```

Commands that change trust refuse to run without a terminal, which keeps a
session's Bash tool from running them. That's a speed bump, so also deny
them in Claude Code's settings; https://peerb.us#security has the rules.

## Rendezvous server

```sh
peerbus rendezvous -listen 0.0.0.0:47615
```

UDP only, and it holds nothing but live registrations. `deploy/` has a
systemd user unit. Point clients at your own with
`{"rendezvous": "host:47615"}` in `~/.config/peerbus/client.json`; invite
codes carry the server, so friends follow automatically.

## Development

```sh
go test -race ./...
```

- `internal/transport` includes a NAT lab: two home networks behind
  masquerading NATs, a relay on a fake internet, built from network
  namespaces inside an unprivileged user namespace, so no root is needed. It
  checks that same-machine, LAN, hole-punched and relayed connections each
  take the expected path. It skips itself where user namespaces aren't
  available.
- `cmd/peerbus/e2e_test.go` builds the binary and drives channel processes
  over MCP stdio the way Claude Code does.
- `spike/` has the go-libp2p vs hand-assembled transport comparison that led
  to the current design, with results in `spike/README.md`.

Layout:

| Path | |
|---|---|
| `cmd/peerbus` | CLI, broker, channel, and rendezvous entry points |
| `internal/channel`, `internal/mcp` | The MCP channel server |
| `internal/transport` | QUIC transport, rendezvous/relay server, STUN, NAT lab |
| `internal/identity`, `internal/pairing` | Keys, peers, permissions, invites |
| `internal/broker`, `internal/client`, `internal/proto` | The original broker |
| `site/` | https://peerb.us (Workers static assets) |

## License

MIT. Not affiliated with Anthropic.
