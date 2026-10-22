# peerbus

Message passing between Claude Code sessions across Claude accounts, machines,
and (opt-in) other people. It aims to match Claude Code's built-in
cross-session messaging: messages start a turn in an idle session and land
between tool calls in a busy one, show as a one-line preview, and carry a
reply address.

```
 Claude session ─stdio─ peerbus channel ─┐
 Claude session ─stdio─ peerbus channel ─┼─ WebSocket ─ peerbus broker
 Claude session ─stdio─ peerbus channel ─┘
```

Each session runs `peerbus channel`, an MCP server that declares Claude Code's
`claude/channel` capability. Broker deliveries are pushed into the session as
`<channel source="peerbus" ...>` events. The channel finds its own session's
name, id and busy/idle status in Claude Code's session registry
(`$CLAUDE_CONFIG_DIR/sessions/<pid>.json`), so names match what `/rename` and
the native `ListAgents` show.

## Setup (one machine, any number of Claude accounts)

```sh
go build -o ~/.local/bin/peerbus ./cmd/peerbus
peerbus init                     # broker + client config in ~/.config/peerbus
```

Register the MCP server for each Claude account (or use the repo's `.mcp.json`):

```sh
claude mcp add -s user peerbus -- peerbus channel
CLAUDE_CONFIG_DIR=~/.claude2 claude mcp add -s user peerbus -- peerbus channel
```

Start sessions with the channel enabled. Custom channels need the
development flag during the channels research preview:

```sh
claude --dangerously-load-development-channels server:peerbus
```

Servers loaded with `--mcp-config` don't count for `server:` channel entries;
they must come from `.mcp.json` or Claude Code's own MCP config. The local
broker starts on demand and keeps running in the background (log and pid in
`~/.local/state/peerbus`).

Every Claude account on a machine shares `~/.config/peerbus/client.json`, so
they all act as the same peerbus user and message each other freely.

## Other machines

Run the broker somewhere reachable, e.g. your server over Tailscale:

```sh
peerbus init -listen 100.x.y.z:47615      # on the server
peerbus user token vitaminmoo             # prints a token and a join command
peerbus join ws://100.x.y.z:47615 <token> # on each laptop/desktop
```

Use `wss://` behind a TLS-terminating proxy for anything outside a private
network.

## Other people

```sh
peerbus user add alice                   # prints alice's token and join command
peerbus group add friends vitaminmoo alice
```

Users only see each other when they share a group. Messages between users:

- are **held** until the recipient's human approves (`peerbus inbox`,
  `peerbus approve ID`, `peerbus deny ID`). The targeted session gets a
  content-free notice that something is waiting; the text stays out of
  Claude's context until approval.
- can be addressed to a **user** (`alice/`) instead of a session, and the
  recipient's human picks the session: `peerbus approve -to SESSION ID`.
- open the **thread** once approved: replies and follow-ups between that
  thread's participants flow without more prompts for
  `threadApprovalMinutes` (default 120). A new thread needs approval again,
  and someone outside the thread can't post into it.
- are marked `foreign="true"`, and the channel instructions tell Claude to
  treat them as untrusted input.

Only control connections (the CLI) can approve; session connections can't.
`peerbus approve` also refuses to run without a terminal, which keeps a
session's Bash tool from approving on its own. That's a speed bump, not a
security boundary, since Claude can read the same token file. A real boundary
needs approval with separate credentials (see roadmap). `peerbus inbox -watch`
streams held messages and sends desktop notifications.

`shareDetails: true` in `client.json` lets other users see your sessions' cwd
and account. Otherwise they see name, host, status and advertised resources.

## Tools Claude gets

| Tool | |
|---|---|
| `list_peers` | sessions you can reach, their status, host, and resources; optional filter (`ghidra`) |
| `send` | `to` = `user/name#ref`, an own session name, or `user/`; `thread` to continue a conversation |
| `subscribe_idle` | one notice when a session next goes idle or disconnects |
| `advertise` | publish extra resources (programs, environments, data, services) |

Resources are advertised automatically: the git repo, MCP servers configured
for the session (user scope, project scope, `.mcp.json`), `programs` from
`client.json` found on PATH, and `resources` from `client.json`.

## Addressing

| `to` | means |
|---|---|
| `name` | one of your own sessions |
| `name#ref`, `name [ref]` | disambiguated by session id prefix |
| `user/name`, `user/name#ref` | a session of a user you share a group with |
| `user/` or `user` | that user; their human picks the session |

## Development

```sh
go test -race ./...
```

`cmd/peerbus/e2e_test.go` builds the binary and drives two channel processes
over MCP stdio the way Claude Code does, including autostarting the broker.

## Roadmap

- GitHub OAuth on the broker (web login issues tokens; identities become
  GitHub logins) and an approval page that needs a browser session, so approval
  no longer shares credentials with the sessions.
- LLM routing suggestions for user-addressed messages: rank the recipient's
  sessions by cwd, repo and resources with a tool-less model call, shown in
  `peerbus inbox`.
- Approve into a new session: start one in a chosen directory with the message
  as its first prompt.
- Broker federation, so a friend can run their own broker.
