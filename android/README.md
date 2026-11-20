# promptshipper — Android app

A floating mic bubble over any app: tap, talk, and the transcript is in an
editable card you can copy or send. Recordings are transcribed by **your own**
promptshipper server (`server/stt`), with the phone's built-in recognizer as the
fallback. The same app captures text to, and picks from, the cross-device
shared clipboard (`server/clip`), offers a snippet picker over the server's
catalog, keeps a searchable history of past dictations, and can attach a
screenshot of whatever is behind it.

This app was ported from [`ZacxDev/dicta`](https://github.com/ZacxDev/dicta)
with its git history (see *Provenance*). [RESEARCH.md](RESEARCH.md) is dicta's
research note on the platform APIs it relies on.

## Install and pair

1. Install the APK from a GitHub Release tagged `android-v*`.
2. Open the app and grant the microphone and "display over other apps"
   permissions from the main screen.
3. On the computer running the server, run `./setup` (see the repository's
   `AGENTS.md`). It prints a QR code. In the app, tap **Pair with a server** →
   **Scan QR code**, or paste the text `./setup --pairing` prints.

There is **no default server**. Until it is paired the app dictates with the
phone's own recognizer, uploads nothing and probes nothing; the shared
clipboard queues captures and does not send them.

### Pairing

The payload is [docs/pairing.md](../docs/pairing.md), version 1:
`PairingPayload` parses it, `PairingWriter` stores both URLs and both tokens
(tokens through the encrypted `TokenStore`, as two separate secrets) all or
nothing, and `PairingActivity` shows both addresses and writes nothing until
you tap **Pair**. A different `v` is refused with "update promptshipper"; an
unrelated QR code is refused by its `type`.

**How the QR code is read, and why there is no scanner library.** The APK has
no runtime dependencies, and a QR decoder is a big one (ZXing core is ~500 KB;
Google's code scanner needs Play services, which a de-Googled phone lacks).
The payload is plain JSON text, so none is needed:

- **Scan QR code** hands off to any installed scanner app that answers the
  ZXing intent `com.google.zxing.client.android.SCAN` (Binary Eye, ZXing
  Barcode Scanner, …) and reads its result. No CAMERA permission — the scanner
  app owns the camera.
- Without one, most phones' camera apps decode QR codes and offer to copy the
  text; **Paste** reads the clipboard (and clears it after pairing if it still
  holds the payload — it contains your tokens).

Manual entry stays in **Settings** (server URL and token, clip URL and token).

## Network security: cleartext only to private IP addresses

The servers speak plain HTTP over a private network, and `./setup` puts
`http://<private ip>:<port>` in the pairing code. Android's
`network_security_config.xml` can only name *domains*; it cannot express an
address range, and the address is not known until you pair. So the platform
permits cleartext and **`PrivateNetwork` is the rule, in one place**:

| URL | allowed |
|---|---|
| `https://` anything | yes |
| `http://` IPv4 in `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` (CGNAT — Tailscale), `127.0.0.0/8` | yes |
| `http://` IPv6 `::1` or `fd00::/8` (incl. Tailscale's `fd7a:115c:a1e0::/48`) | yes |
| `http://` any other address | **no** |
| `http://` any **hostname** — including `localhost`, Tailscale MagicDNS `*.ts.net`, `*.local`, `*.lan` | **no** |

Hostnames are refused for cleartext because the name is resolved *after* the
check, so the check cannot know where the bytes go. A tailnet name gets a real
certificate from `tailscale serve` / `tailscale cert`, so use
`https://<host>.ts.net`; `./setup` emits the tailnet **IP**, which needs neither.

The rule is applied when a URL is **stored** (`BaseUrlRule`: pairing, Settings,
the debug-build config door) and again when a connection is **opened**:
`PrivateNetwork.open` is the default connection factory of both HTTP clients,
refuses the URL with an `IOException` (shown as "could not reach the server"),
and turns off redirect following so a private host cannot bounce a request —
bearer token included — to a public `http://` URL. `NetworkManifestTest` fails if
any other file opens a connection.

## Your agents and the clip server

Two settings in **Settings → Shared clipboard** connect the phone to agents
that use the clip server (for example through the MCP server):

- **Send each dictation to my clip feed**: **off by default**, and offered
  unchecked on the pairing screen. When it is on, each finished transcript is
  also queued to the clip server (device `phone-dictation`) through the same
  outbox as a share-sheet capture, with the same retry and backoff. When it is
  off, nothing about a dictation leaves the phone except the transcription
  upload.
- **Let my agents push to my clipboard**: **on by default** once paired. While
  the bubble is on, the app long-polls the clip server's change feed
  (`GET /v1/clips?after=<seq>&wait=50`, see `server/clip/README.md`). Every
  clip posted with device **`mcp`** is copied to the clipboard and shows a
  short "From your agent" notice. Clips from any other device are never
  copied.
  - The cursor (the largest `seq` seen) is persisted.
  - When the listener restarts with a saved cursor (after the bubble or screen
    was off), it reads what is already pending and copies **only the newest
    `mcp` clip** of that backlog, then moves past all of it. Pushes that arrive
    while it runs are copied as usual.
  - A poll reads at most 20 clips and at most 2 MiB; a larger answer counts
    as a failure and is never partly applied.
  - A result from a clip server that was replaced while the poll was open
    (a re-pair) is discarded.
  - On the first poll, the app starts at the server's `head`, so existing
    history is never copied.
  - A cursor ahead of `head` (the store was restored from an older backup) is
    reset to `head` without copying.
  - Pairing again, or changing the clip URL, resets the cursor.
  - Failures back off from 2 s, doubling, up to 5 min. A `429` waits for its
    `Retry-After`.
  - Polling stops when the bubble stops, or five minutes after the screen
    goes off. It resumes when the screen comes back on. No wake lock is held.
  - Turning the setting off or on, pairing, or changing the clip URL takes
    effect immediately while the bubble runs. A service that starts with the
    screen off (at boot) gets the same 5-minute grace.

## History import from dicta

Settings → **Import history from a file** reads the archive format in
[docs/history-archive-format.md](docs/history-archive-format.md) (dicta PR #55,
ported at `a7bb3e6`), so a history exported by the dicta app imports unchanged.
The whole archive is read and verified (manifest, every entry's SHA-256 and
length) before anything is written; existing dictations are never overwritten;
an item whose id already exists is skipped. **Export** writes the same format.
⚠ Import does not apply the retention policy, but the next new dictation does
(365 days / 1000 items / 2 GiB, whichever binds first), so imported items older
than a year are removed then — see the format doc, §8.

## Build and test

NixOS (the dev shell pins nixpkgs, JDK 17, Gradle 8 and SDK 34):

```sh
cd android
nix-shell android.nix --run './gradlew :app:testDebugUnitTest --tests "dev.zacx.promptshipper.PairingPayloadTest" \
  -Pandroid.aapt2FromMavenOverride=$ANDROID_HOME/build-tools/34.0.0/aapt2'
```

The aapt2 override is required on NixOS (AGP's downloaded aapt2 cannot exec
there). Elsewhere, JDK 17 and an Android SDK are enough: `./gradlew
:app:testDebugUnitTest`. Run targeted classes locally; the whole JVM suite runs
in CI (`.github/workflows/android.yml`), which asserts a minimum test count from
the JUnit XML rather than trusting the task's exit status.

Debug builds are signed by `~/.android/debug.keystore` (override with
`-Ppromptshipper.debugKeystore=…`); a missing keystore fails the build rather
than letting AGP mint one.

## Release signing

Releases are the **release** build type — **not debuggable** (dicta published a
debuggable debug build; this app does not) — signed by a key generated for this
app. The key is read only from the environment:

```
PROMPTSHIPPER_RELEASE_KEYSTORE        path to the .jks (kept outside every repository)
PROMPTSHIPPER_RELEASE_STORE_PASSWORD
PROMPTSHIPPER_RELEASE_KEY_ALIAS
PROMPTSHIPPER_RELEASE_KEY_PASSWORD
```

With any of them missing, `assembleRelease` fails at configuration time — never
an unsigned or default-signed APK. Its certificate's SHA-256 is pinned in
`.github/workflows/android-release.yml`, and that workflow will not publish an
APK whose signer differs (`scripts/assert-apk-signer.sh`) or that is debuggable
(`scripts/assert-apk-not-debuggable.sh`).

To cut a release: bump `versionName`/`versionCode` in `app/build.gradle.kts`
(`versionCode = major*10000 + minor*100 + patch`), merge, then push a tag
`android-v<versionName>`. The workflow needs these repository secrets:

| secret | value |
|---|---|
| `ANDROID_RELEASE_KEYSTORE_B64` | `base64 -w0` of the release `.jks` |
| `ANDROID_RELEASE_STORE_PASSWORD` | its store password |
| `ANDROID_RELEASE_KEY_ALIAS` | the key alias |
| `ANDROID_RELEASE_KEY_PASSWORD` | the key password |

🔴 Losing the keystore means no update can ever be installed over a published
build (Android refuses a different signer, and the way through is an uninstall,
which deletes the on-device history). Back it up.

## What only a device can verify

The JVM suite (Robolectric) cannot run AndroidKeyStore, MediaCodec, the
overlay window manager or a real camera/scanner hand-off. Before trusting a
release on a handset, check:

1. **Pairing by scan** — with a ZXing-intent scanner installed (e.g. Binary
   Eye), tap Pair with a server → Scan QR code, scan `./setup --pairing`'s code,
   confirm both addresses, tap Pair. The main screen's Server row names the
   host. Without a scanner, the "No QR scanner app answered" message appears,
   and camera-app copy → Paste works.
2. **Dictation** — tap the bubble, speak, tap again: the transcript comes from
   the server (Settings → Test connection first). Then point the STT URL at an
   unreachable private IP and confirm the phone's recognizer takes over.
3. **Cleartext rule** — enter `http://<a hostname>:8118` in Settings: it is
   refused before saving.
4. **History import** — export from dicta (Settings → Export history to a file),
   import the `.zip` here, and confirm the item count and that a re-import
   skips everything.
5. **Release APK** — `adb shell run-as dev.zacx.promptshipper ls` must fail
   ("package not debuggable").

`app/src/androidTest` holds device tests (`LiveServiceDeviceTest` needs a paired
server: push the URL and token to `/data/local/tmp`, see its kdoc). CI compiles
them; it does not run them.

## Provenance

`android/` was imported from the public `ZacxDev/dicta` with
`git filter-repo --to-subdirectory-filter android` (main at `569ce90`), so its
build-story commits are in this repository's history, followed by the
history-export/import commits of dicta PR #55. Identity changed in this repo:
package and applicationId `dev.zacx.promptshipper` (a new app — it installs
beside dicta and shares none of its data), app name, a new release key, no
hardcoded endpoints. Some internal names (`DictaFileProvider`, the `Dicta` theme
and colour names, preference file names) were kept: nothing user-visible, and
nothing has to migrate for a new app.
