# peerbus for Android

`peerbus tui` in your pocket: every message held on any of your machines,
with the full text, to approve, route to any of your sessions, or
decline; messages written to you (`user/human`), to read, reply to or
dismiss; the history of every message, yours and friends'; writing to
any session you can reach; and a notification when something new waits.

The phone pairs as one of your own devices and runs the same Go code as
the CLI (`../mobile`, bound with gomobile): while the app is open it joins
the mesh as a control client, like `peerbus tui`. In the background it's
off the mesh, and your sessions push it instead (below).

## Pairing

On a computer that's already set up:

```sh
peerbus phone
```

It prints a QR code; in the app, tap **Scan code** (or paste the code
printed under it). The phone gets your identity key and peer list, as
`peerbus invite -self` gives a new machine. On the mesh it is you,
`user/human`, as `peerbus tui` is: what you write from it comes from
there, and replies come back to it. **Forget this phone** on the
**Unit** tab removes the key and peers again.

## Notifications

Sessions don't keep the phone awake. When a session holds a message (or
sees one approved, routed or declined), it seals a short note (who it's
from and to, and the start of the text) with AES-256-GCM under a key
derived from your self secret, which only your devices hold, and asks the
rendezvous server to forward it to the phone's Firebase Cloud Messaging
token. The phone decrypts it and shows or clears the notification without
joining the mesh. Google and the rendezvous operator see a token and
ciphertext.

The phone's token is kept with your identity in `peers.json`: it
registers with any one session when the app opens, and your machines
pass it on to each other. `peerbus id` and `peerbus peers` show it.

Pushes need:

1. A Firebase project with an Android app for the package `us.peerb.app`.
2. `android/firebase.properties` with that app's values (copy
   `firebase.properties.example`; it's gitignored). Builds without it work,
   and only notify while the app is open.
3. A rendezvous server holding a service account key for the project
   (Project settings → Service accounts → Generate new private key):

   ```sh
   peerbus rendezvous -fcm-credentials /path/to/service-account.json
   ```

   A server without it answers push requests with an error, and sessions
   carry on without pushing.

## Building

Needs Go, a JDK, and the Android SDK with an NDK (`~/Android/Sdk`, or set
`ANDROID_HOME`). gomobile is pinned in `../go.mod` as a tool; Gradle runs
`build-core.sh` to bind the Go core before compiling.

```sh
cd android
./gradlew assembleDebug        # app/build/outputs/apk/debug/app-debug.apk
./gradlew installDebug         # onto a connected device
```

The Go side of the app is tested in `../mobile` (`go test ./mobile`),
including pairing, routing and the push round trip against a fake
Firebase.
