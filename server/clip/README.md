# clip — the shared clipboard server

The server half of the cross-device shared clipboard. Highlight text on any
device, it is on every other device. The Android app captures to it and reads
from it; the desktop `snip` tool and the MCP server (later phases) do too.

Deployed by `deploy/compose/` (see the repo's `AGENTS.md`). The image is
`ghcr.io/zacxdev/promptshipper-clip`; the binary inside it is still `clipstore`.

## Shape

Single Go binary, distroless static, one SQLite file with an FTS5 index, three
bearer-gated HTTP operations plus an unauthenticated `/healthz`. **No TLS of its
own**: reach it over a private network (a Tailscale tailnet, your LAN, a VPN),
and never publish it to the internet without a TLS-terminating proxy in front.

## API

Every operation that reads or writes a clip requires
`Authorization: Bearer <token>`. **`/healthz` does not** — it is the probe
endpoint and nothing else; see "`/healthz` is unauthenticated" below. Errors are
`{"error":"<stable-code>","detail":"<prose>"}`.

| | |
|---|---|
| `POST /v1/clips` | `{"body":…,"device":…}` → `201` with the clip, or `200` with `"deduped":true` when an identical body already existed and was bumped to the front |
| `GET /v1/clips?limit=N&q=…` | `{"clips":[…],"limit":N,"query":"…"}`, newest first |
| `GET /v1/clips?after=SEQ&wait=S&limit=N` | **the change feed**: `{"clips":[…],"limit":N,"head":H}`, clips with `seq > SEQ`, **in `seq` order**; see *Change feed* |
| `DELETE /v1/clips/{id}` | `204`, or `404 no_such_clip` |
| `GET /healthz` | `{"status":"ok"}`, or `503 store_unreadable`. **Probe-only, and the one route that needs no token.** Still reads a row. |

### `limit`

`limit` is **the only growth bound this service has**: the design decision is no
expiry and manual delete only, so the table is unbounded forever.

* default **100** when `?limit=` is absent (an empty `?limit=` counts as absent)
* hard cap **500**
* a `limit` outside `1..500` is **refused with `400 invalid_limit`, not clamped**.
  A clamp would hand the client 500 rows with a `200` and no way to tell a
  truncated newest-first list from a store that holds only 500 rows.

### `q`

A clip matches when **every whitespace-separated query token appears somewhere in
its body as a case-insensitive substring**. `q=assw` finds `password`.

That is the whole contract, and it is uniform — but it is served two ways, and
the split is a correctness fix rather than an optimisation:

| every token ≥ 3 characters | the FTS5 index, `tokenize='trigram'` |
|---|---|
| **any** token shorter than 3 | a `LIKE '%…%' ESCAPE '\'` table scan |

> 🔴 **Why the scan exists.** A trigram index cannot answer a 1- or 2-character
> query: it returns **zero rows and no error**, which is indistinguishable from
> "nothing matched" — the exact confusion the *Fails loud, never empty* section
> below exists to prevent. So a short token sends the **whole** query to the
> scan, because AND-ing a MATCH with a LIKE would mix two notions of matching.
> At this scale (a personal clipboard) the scan is cheap.
>
> The floor is counted in **characters, not bytes**: a 2-character CJK query is
> 6 bytes and still cannot be served by the index.

Punctuation is **indexed** under trigram, so `q=----` is a real search that finds
a clip whose body is `----` — it is no longer discarded. A query that genuinely
matches nothing is an **empty result with a `200`**, never an error.

No user text reaches either grammar. Every token is re-emitted as an FTS5 quoted
phrase with `"` doubled, so `NEAR`, `AND`, `body:` and parentheses are
literalised; on the scan path `%`, `_` and `\` are escaped, so **`q=100%` matches
only a body containing `100%`** rather than every row in the store.

> ⚠ **One measured divergence between the two paths.** Trigram case-folds beyond
> ASCII; SQLite's `LIKE` folds ASCII only. Against a body of `Ärger über Äpfel`,
> `q=ärger` (index) finds it and `q=ärger üb` (scan, because `üb` is short) does
> not. Every ASCII query — effectively all of them — agrees on both paths. See
> `PlanSearch`'s comment for the measurement and
> `TestNonASCIICaseFoldingDivergesBetweenTheTwoPaths` for the guard.

Changed from the first cut of this service, which was prefix-only (`q=assw` did
*not* find `password`). A database created by that version is migrated on the
next start: `CREATE VIRTUAL TABLE IF NOT EXISTS` cannot alter an existing table,
so `OpenStore` reads the stored DDL and drops and rebuilds the index when it is
not trigram. The index holds no bodies, so a rebuild loses nothing.

### Change feed (long-poll)

`GET /v1/clips?after=<seq>&wait=<seconds>` is how a device learns about new
clips without hammering the list:

* Every clip carries **`seq`**: a counter that only ever grows. It is set when
  a clip is created **and again every time an identical body is re-POSTed**
  (a dedup bump), and a deleted row's `seq` is never reused.
* The feed returns every clip with `seq > after`, **in `seq` order** (oldest
  change first), up to `limit` (default 100, max 500) — **as soon as at least
  one exists**. A re-pushed body is therefore delivered again, after the
  cursor, even though its `id` is unchanged.
* With `wait` (0–60 s, default 0) and nothing new, the request is held open
  and answers the moment a clip is POSTed (created or bumped), or
  `{"clips":[]}` when `wait` expires. Never `null`.
* **The cursor is the largest `seq` you have seen** — poll with
  `after=<last seq>`. Start with `after=0`. To skip history, read the feed with
  `wait=0` until it returns fewer than `limit` rows and keep its largest `seq`.
  Never use `id` as the cursor.
* **Every feed answer carries `head`**: the counter's current value, i.e. the
  largest `seq` ever assigned (deleted rows included), even when `clips` is
  empty. **Contract: if your cursor is greater than `head`, reset it to
  `head` and treat that as a resync** — the store was restored from an older
  backup or replaced by a fresh volume, so clips you already hold may be gone
  and later clips will reuse lower numbers. A request with `after > head`
  answers at once, whatever `wait` says, so it never waits forever.
* **Deletes are not in the feed.** A deleted clip simply stops existing; no
  event is sent for it.
* ⚠ **Downgrade note:** a previous binary (from before `seq`) can run against
  a migrated database, but every clip it writes gets `seq = 0`, which the
  feed never delivers. Upgrading again does not renumber them; they stay
  visible in the newest-first list. (The `seq` index is deliberately not
  UNIQUE, so such rows never break later POSTs.)
* `seq` is not the list order: `GET /v1/clips` without `after` is still the
  newest-first list by `created_at`, unchanged.
* A database created before `seq` existed is migrated on start: existing rows
  get `seq = id` (so a cursor taken from the old id-based feed keeps its
  place), and new values start above every id ever issued.
* `after` cannot be combined with `q` (`400 invalid_feed`); `wait` without
  `after`, or outside 0–60, is `400 invalid_wait`; a negative or non-numeric
  `after` is `400 invalid_after`.
* At most 64 requests wait at once; one more gets **`429 too_many_waiters`**
  with `Retry-After: 5`, immediately. A non-waiting read (`wait=0`) is never
  refused.
* Bearer-gated like every clip route. A waiter holds no database connection
  while it waits (one short `SELECT` per wake-up), so it cannot block writes
  on the single SQLite writer. The server's write deadline (90 s) outlasts the
  longest wait, and shutdown releases every waiter with an empty answer.

Long-poll rather than SSE: one ordinary request/response per poll survives
flaky mobile networks, proxies and an Android foreground service being
suspended, with no reconnection protocol to get right.

#### The auto-copy contract (for a feed-aware app)

The Android app consumes this feed: while its bubble service is in the
foreground and its agent-push setting is on (the default), it long-polls
`GET /v1/clips?after=…&wait=…` (`ClipPushListener` / `ClipPush.kt`) and
**auto-copies to the system clipboard ONLY clips whose `device` is exactly
`mcp`** — text an agent pushed via the MCP server. Every other clip (captured on
a phone, a laptop, by snip) is listed but never copied unasked. On its first
start it begins at `head` and copies nothing; after a gap it copies only the
newest `mcp` clip of the backlog. So:

* the MCP server (and anything else meant to land on the phone's clipboard)
  POSTs with `"device":"mcp"`;
* nothing else may use `mcp` as its device name;
* the server does not enforce either rule — `device` is a free string — so the
  contract lives with the clients and this paragraph.

### The server never stores a server token

A clip whose `body` or `device` contains this server's own bearer token
(`CLIPSTORE_TOKEN`), or any credential listed in `CLIPSTORE_REFUSE_TOKENS`, is
refused with **`422 contains_server_token`** — "the clip looked like a server
access token and was not stored". It is not stored, the body is never echoed,
and neither the answer nor the log line says which token matched. Reason: a
token copied on the phone and captured as a clip would otherwise be readable
back out by anything that can read clips (e.g. an agent via the MCP server).
Any other value, including other 64-hex strings, is stored as usual.

`CLIPSTORE_REFUSE_TOKENS` is how the clip server learns the **STT server's
token**, which the same phone holds. The compose stack sets it to
`STT_API_TOKEN` from `.env` (so `./setup` covers it with no extra step), and the
Helm chart, when `stt.enabled`, sets it by `secretKeyRef` to the STT Secret's
`stt.auth.tokenKey` (`optional: true`; the value never appears outside the
Secret). Running clip alone, set it yourself (e.g. `clip.extraEnv` with a
`secretKeyRef`).

At startup the server counts stored clips that contain its token, and each
`CLIPSTORE_REFUSE_TOKENS` entry (rows saved before this guard existed) and, if
there are any, logs **only the count** (per entry by position, never the value)
and the fix: **rotate the token** with `./setup --rotate` (it rotates both), then
re-pair your devices. Deleting those clips is not the fix — a token that sat in
a readable clip may already have been read, and only rotation retires it.
(After rotation the old value is no longer a credential; delete the clips with
`DELETE /v1/clips/{id}` if you want them gone.)

⚠ **Not covered:** the STT server's *extra* named tokens (`STT_API_TOKENS`).
The chart and compose pass only `STT_API_TOKEN`. With compose (or clip running
alone), add the others to `CLIPSTORE_REFUSE_TOKENS` by hand as one
comma-separated list of raw values — the main token included, since the
variable is replaced, not appended to. With the Helm chart and `stt.enabled`
there is **no** supported way to add them: the chart sets
`CLIPSTORE_REFUSE_TOKENS` itself, and it **refuses to render** if
`clip.extraEnv` also names it (a duplicate env name would make Kubernetes keep
the last entry and silently drop the STT-token refusal). Under Helm, extra STT
tokens stay uncovered.

### Fails loud, never empty

An unreadable or unwritable store answers **5xx with a named reason and no
result field at all** — never `[]`. An empty result is indistinguishable from a
working feature with nothing in it, and both clients are built on this server not
making that mistake.

An **empty store**, by contrast, is a `200` with `{"clips":[],"limit":100}` —
never `null`, which would throw in a client that iterates it.

### Dedup

`UNIQUE(body_sha256)` plus `INSERT … ON CONFLICT (body_sha256) DO UPDATE SET
created_at = …, device = …, bump_count = bump_count + 1 RETURNING …` — **one
atomic statement**, so two concurrent captures of the same body cannot both miss
a read-then-write check.

`bump_count` is how `"deduped"` is derived: an upsert reports the same
`changes()` whichever path it took, but a fresh `INSERT` leaves `bump_count` at
its `DEFAULT 0` and only `DO UPDATE` can raise it. A bump re-attributes the clip
to the device it most recently came from.

> ⚠ **Clip ids have gaps, and that is normal.** `AUTOINCREMENT` allocates a
> rowid before the conflict is resolved, so a POST that turns out to be a dedup
> bump consumes an id nothing uses. Ids are opaque handles for `DELETE`; nothing
> may read order or count out of them (`created_at DESC, seq DESC` is the order).

## Storage

* One SQLite file at `-db` (default `/data/clipstore.db`); compose keeps `/data`
  on the named volume `clip-data`.
* `journal_mode=WAL`, so an abrupt container stop is crash-safe.
* `SetMaxOpenConns(1)`: one writer in the process, which removes `SQLITE_BUSY`
  from the failure set. The cost — a slow query blocks everything including
  `/healthz` — is stated in `store.go` and is acceptable for a personal
  clipboard of short rows.
* 🔴 **Run exactly one clipstore per database file.** Never point two
  containers at one volume.
* Back up through SQLite's online backup API, not `cp`, because of WAL:
  `sqlite3 clipstore.db ".backup clipstore-backup.db"`.

## `/healthz` is unauthenticated, on purpose

So probes (compose, Kubernetes, `./setup`) are a plain HTTP GET that carries no
secret. What it discloses is **one bit** — this process can read its database —
and no clip content, no row count and no route names. The bearer requirement
covers every route that reads or writes a clip.
`TestTheRouteLedgerIsExactlyTheseRoutes` pins the route set on each side of that
boundary (it fails if either side grows *or* shrinks), and
`TestOnlyHealthzIsReachableWithoutTheBearerToken` drives every route over HTTP.

🔴 **It is a SCHEMA probe, not a storage probe.** `Store.Ping` runs
`SELECT count(*) FROM clips` rather than `db.Ping()`, so a 200 means the `clips`
table still resolves — it catches a lost or half-migrated schema. It does
**not** detect a deleted, truncated or corrupted file under an already-open
store: SQLite serves `count(*)` from its page cache (measured against
`modernc.org/sqlite v1.51.0`; the table is on `Store.Ping` in `store.go`).

## Configuration

| | |
|---|---|
| `$CLIPSTORE_TOKEN` | the bearer token. **Required**, minimum 24 bytes; a shorter or absent one is `exit 2` at startup, never a default and never an open listener. `./setup` generates one into `deploy/compose/.env`. |
| `$CLIPSTORE_REFUSE_TOKENS` | optional, comma-separated: other credentials a clip must not contain (default install: the STT token — see *The server never stores a server token*). An entry shorter than 24 bytes is **not** checked and is reported at startup by position and length; a short needle would refuse ordinary clips, and another service's misconfiguration must not stop this one starting. |
| `-addr` | default `:8139` |
| `-db` | default `/data/clipstore.db` |

## What is deliberately absent

* **No expiry sweep.** Manual delete only. If expiry is ever added it belongs
  in-process on write+startup, **never** as a separate cron container — that
  would be a second *write-capable* process against a single-writer WAL file.
* **No labels or categories.** This is a clipboard. The labelled store is the
  snippet catalog served by the STT server.
* **No TLS and no public exposure.** See *Shape*.
* **No server→phone push channel** beyond the long-poll change feed above.

## Local loop

```sh
make gate     # what CI runs: CGO off, vet + test
make test     # the -race run, which CI deliberately does not do
make build && CLIPSTORE_TOKEN=$(openssl rand -hex 32) ./clipstore -db /tmp/c.db
```

## Tests

🔴 **Almost everything here is an invariant guard / characterization test, not
regression coverage.** The test files say so in their own headers, and they name
the handful of guards that *can* be watched fail together with the mutation that
does it.

## Image

Built from this directory's `Dockerfile`. `deploy/compose` builds it locally;
release images are pushed to GHCR by CI on a `v*` tag
(`ghcr.io/zacxdev/promptshipper-clip`).
