# Pairing payload — version 1

`./setup` prints a QR code and the same content as a line of text
(`PAIRING_PAYLOAD=<json>`). Scanning it is how a phone learns where the servers
are and which tokens to use, with nothing typed by hand. This document is the
contract between `./setup` (the producer) and the Android app (the consumer).
`tests/test_setup.py::test_the_pairing_payload_is_exactly_the_v1_shape` pins the
producer side.

## Encoding

* The QR code's content is the payload **as UTF-8 JSON text**, a single object,
  no surrounding whitespace, no URI scheme, no base64. (A scanner app that shows
  the raw text therefore shows the JSON; the app parses it directly.)
* Byte mode, error-correction level of the producer's choosing. A v1 payload is
  ~280 bytes, which fits comfortably in a QR code a phone camera reads off a
  terminal.

## Schema (v1)

```json
{
  "v": 1,
  "type": "promptshipper-pairing",
  "name": "my-desktop",
  "stt":  { "url": "http://100.101.102.103:8118", "token": "<64 hex chars>" },
  "clip": { "url": "http://100.101.102.103:8139", "token": "<64 hex chars>" }
}
```

| field | type | rule |
|---|---|---|
| `v` | integer | **The format version. `1` for this document.** |
| `type` | string | always `"promptshipper-pairing"`. Lets the app reject an unrelated QR code with a clear message instead of a parse error |
| `name` | string | a human label for the server (the producer uses the machine's hostname, reduced to `[A-Za-z0-9._-]`, ≤ 63 chars). Display only; never used to connect. May be absent in a hand-written payload — the app then shows the STT host |
| `stt.url` | string | base URL of the STT server |
| `stt.token` | string | bearer token for every STT request except `GET /healthz` |
| `clip.url` | string | base URL of the clip server |
| `clip.token` | string | bearer token for every clip request except `GET /healthz` |

### URL rules

* Scheme `http` or `https`, then a host and a port, **and nothing else**: no
  path, no query, no fragment, no trailing slash in what the producer emits.
  The client appends `/v1/audio/transcriptions`, `/v1/snippets`, `/v1/clips`
  and `/healthz` itself, so a path in the base would break every request. (The
  app already refuses a clip base URL with a path; the same rule applies to both.)
* An IPv6 host is bracketed: `http://[fd7a:115c:a1e0::1]:8118`.
* The producer emits `http://` because the servers have no TLS of their own and
  are meant to be reached over a private network. A consumer that restricts
  cleartext should allow it only for private ranges (RFC 1918, the CGNAT range
  `100.64.0.0/10` that Tailscale uses, `fd00::/8`, loopback) and require
  `https://` for anything else.

### Token rules

* Each token is the exact string to send after `Authorization: Bearer ` — no
  trimming, no re-casing, no encoding.
* The producer generates 32 random bytes as 64 lowercase hex characters. A
  consumer must accept any token of **24 or more** characters from
  `[A-Za-z0-9._~-]` (clip refuses to start with fewer than 24 bytes).
* `stt.token` and `clip.token` are different secrets and must be stored
  separately.

## Compatibility

* **Unknown fields are ignored** at every level. A producer may add fields in v1
  without a version bump (e.g. a future `mcp` object) as long as every field
  above keeps its meaning.
* **A different `v` is rejected** with a message telling the user to update the
  app — never parsed on a best-effort basis. Any change that removes a field or
  changes a field's meaning bumps `v`.
* Missing `stt` or `clip` (or a missing `url`/`token` inside one) is a malformed
  payload in v1; the app must not pair half a server silently.

## Security

The payload **contains both bearer tokens**. Treat the QR code and the
`PAIRING_PAYLOAD=` line like a password: show it only on a screen you trust,
don't paste it into a chat, a ticket or a public log. If it leaks, run
`./setup --rotate` and re-pair every device.

`./setup --pairing` re-prints the code from the existing configuration without
changing anything. `./setup --qr-png <path>` (alone or with `--pairing`) writes
the same QR code to a PNG file with mode 0600 and prints **only the path** —
neither the payload nor a token reaches stdout or stderr. That is the mode an AI
agent installing on the user's behalf must use.
