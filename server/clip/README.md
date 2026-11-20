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
> may read order or count out of them (`created_at DESC, id DESC` is the order).

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
| `-addr` | default `:8139` |
| `-db` | default `/data/clipstore.db` |

## What is deliberately absent

* **No expiry sweep.** Manual delete only. If expiry is ever added it belongs
  in-process on write+startup, **never** as a separate cron container — that
  would be a second *write-capable* process against a single-writer WAL file.
* **No labels or categories.** This is a clipboard. The labelled store is the
  snippet catalog served by the STT server.
* **No TLS and no public exposure.** See *Shape*.
* **No server→phone push.**

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
