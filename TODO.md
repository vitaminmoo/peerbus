# TODO

## Push peer edits from the CLI

Your machines sync peers.json through their running sessions. An edit made
with `peerbus peer` on a machine with no session open waits until one
starts there. The CLI could join the network briefly, as `peerbus peers`
does, and push the change itself.

## Show where peer changes came from

Synced changes apply silently. `peerbus peer list` could say which machine
last changed each peer, and a session could get a notice when a sync adds
or removes someone, since a compromised machine can push peers to the
others (within what the shared identity key already allows).

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
