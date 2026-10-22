# peerbus

Message passing between Claude Code sessions across Claude accounts,
machines, and friends you've paired with. Setup guide: https://peerb.us

Claude Code's built-in cross-session messaging covers sessions under one
account on one machine. peerbus covers the rest. Messages land the same
way: between tool calls in a busy session, as a new turn in an idle one,
with a one-line preview in the terminal and a thread to reply on.

## Status

Early. Everything below works end to end on Linux; commands and file
formats may still change.

| Piece | State |
|---|---|
| Channel server and its tools (`peerbus channel`) | Working, on the P2P transport |
| Same machine, any Claude account | Working, no server needed |
| Your other machines and friends' machines | Working, via a rendezvous server |
| P2P transport: QUIC, hole punching, relay, STUN | Working and tested |
| Identity, invites, pairing, permissions | Working |
| Held messages: `peerbus tui` across all your machines, `inbox` / `approve` / `deny` on one | Working |
| Rendezvous server (`peerbus rendezvous`) | Working; public instance at `rv.peerb.us:47615` |

## How it works

```
Claude session ─stdio─ peerbus channel ══ QUIC ══ peerbus channel ─stdio─ Claude session
                              └──── rendezvous server: introductions, relay ────┘
```

- Each session runs `peerbus channel`, an MCP server that declares Claude
  Code's [`claude/channel`](https://code.claude.com/docs/en/channels-reference)
  capability, so incoming messages are pushed into the session.
- Sessions connect to each other over QUIC (TLS 1.3) with each side pinned to
  the other's Ed25519 key, one connection per pair of sessions. One UDP socket
  per session carries both QUIC and peerbus's control packets (RFC 9443
  multiplexing).
- Paths, most direct first: loopback on the same machine (found through a
  directory under `~/.local/state/peerbus`, no server), LAN addresses, hole
  punching across NATs, then the relay. A STUN query finds the real public
  address when the rendezvous server shares your NAT.
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
  whether they see your working directories and advertised resources. Held
  messages wait in `~/.local/state/peerbus/inbox.json` until you approve
  them in `peerbus tui` (any of your machines) or with `peerbus approve`.

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

Sessions on one machine now see each other. For more machines and for
friends:

```sh
peerbus id -name yourname     # your fingerprint and the name you suggest
peerbus id -machine laptop    # name this machine (default: its hostname)
peerbus invite -self          # add one of your own machines
peerbus invite                # invite a friend; send them the code privately
peerbus pair pb1-…            # redeem a code (on the other side)
peerbus peer list
peerbus peer set alice deliver=auto sessions='ghidra-*,desk/*'
```

Your machines keep one peer list: pairing, permission changes and
removals on one reach the others whenever their sessions are connected,
with the latest change to each peer winning. A `sessions` glob containing
`/` matches `machine/session`, so `desk/*` limits a friend to one machine.

Commands that change trust refuse to run without a terminal, which keeps a
session's Bash tool from running them. That's a speed bump, so also deny
them in Claude Code's settings; https://peerb.us#security has the rules.

Tools Claude gets:

| Tool | |
|---|---|
| `list_peers` | Reachable sessions, their status, host and path (local, direct, relay), and what they advertise (MCP servers, programs, repos). Optional filter. |
| `send` | Plain text to a session; pass `thread` to continue a conversation. |
| `subscribe_idle` | One notice when a session next finishes its turn or disconnects. |
| `advertise` | Publish extra resources this session can offer. |

The CLI does the same from a shell: `peerbus peers`, `peerbus send`,
`peerbus inbox`, `peerbus approve`, `peerbus deny`.

`peerbus tui` is where the human handles held messages: every one on all
your machines, with the full text, to approve, decline, or route to any of
your sessions on any machine. Nothing in it reaches an agent until you act.
`tab` switches to the history: every message your sessions sent and
received in the last 30 days, on every machine, and what became of it;
`f` narrows it to messages between your own sessions or with friends.
Each machine keeps its part in `~/.local/state/peerbus/history.jsonl`,
readable only by you, like the inbox.

## Rendezvous server

```sh
peerbus rendezvous -listen 0.0.0.0:47615
```

UDP only, and it holds nothing but live registrations. `deploy/` has a
systemd user unit. Point clients at your own with
`{"rendezvous": "host:47615"}` in `~/.config/peerbus/client.json`; invite
codes carry the server, so friends follow automatically. `"none"` keeps
sessions to one machine.

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
- `internal/mesh` tests run several sessions of two paired users in one
  process against a local rendezvous server: discovery, delivery, holding and
  approving, idle notices, and permission changes picked up live.
- `cmd/peerbus/e2e_test.go` builds the binary, runs a rendezvous server, and
  drives channel processes over MCP stdio the way Claude Code does, plus the
  CLI against them.
- `spike/` has the go-libp2p vs hand-assembled transport comparison that led
  to the current design, with results in `spike/README.md`.

Layout:

| Path | |
|---|---|
| `cmd/peerbus` | CLI, channel, and rendezvous entry points |
| `internal/channel`, `internal/mcp` | The MCP channel server |
| `internal/mesh` | Sessions finding and talking to each other; delivery policy |
| `internal/transport` | QUIC transport, rendezvous/relay server, STUN, NAT lab |
| `internal/identity`, `internal/pairing` | Keys, peers, permissions, invites |
| `internal/inbox` | Held messages and thread approvals |
| `internal/history` | Messages each machine's sessions sent and received |
| `internal/proto` | Frames sessions exchange |
| `site/` | https://peerb.us (Workers static assets) |

## License

MIT. Not affiliated with Anthropic.
