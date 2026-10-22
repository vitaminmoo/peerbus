# TODO

## Sync peers between your own machines

Each machine has its own `peers.json`, copied once by `peerbus pair` from
`invite -self`. A friend paired on one machine afterwards only reaches that
machine until the others re-pair with `-force`.

Own devices already hold live links to each other, so they could exchange
peer-list changes over them. Open questions:

- Opt in or on by default? A machine might deliberately carry fewer peers,
  for example a work laptop that shouldn't reach a friend's sessions.
- Per-machine policy instead of a switch: which peers a machine accepts
  from the others, or a flag per peer ("this machine only").
- What syncs: additions, removals, renames, `peer set` permission edits,
  `enable`/`disable`. Removals and disables propagating matters most, since
  revoking a friend should revoke them everywhere.
- Conflicts when two machines edit the same peer while apart: last writer
  wins by timestamp, or keep both and ask.
- Trust: a compromised machine could push a peer to all the others. That's
  within what the shared identity key already allows, but syncing changes
  should still be visible (`peerbus peer list` showing where a peer came
  from, a notice on the receiving machine).

## Say why pairing hangs

When the rendezvous name doesn't resolve (or the server never answers),
`peerbus pair` prints `Pairing...` and waits out its 2-minute timeout
silently. `pairing.Options.Log` is never set, so the transport's
`resolve ...` messages are dropped. Print resolve and registration failures,
and fail fast when the name doesn't resolve at all.

## Terminal checks for `send` and `inbox`

`peerbus send` isn't held by `send=ask` and `peerbus inbox` prints held
messages in full, and neither requires a terminal, so a session's Bash tool
can use them. The site tells people to deny both in Claude Code's settings;
the CLI should also refuse without a terminal, like `approve` does.
