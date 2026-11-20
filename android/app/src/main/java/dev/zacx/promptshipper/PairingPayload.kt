package dev.zacx.promptshipper

import org.json.JSONException
import org.json.JSONObject
import org.json.JSONTokener

/**
 * The pairing payload, version 1 — the consumer half of `docs/pairing.md`.
 *
 * `./setup` prints it as a QR code and as a line of text; the app reads it by
 * scanning or pasting, and learns both servers' URLs and tokens with nothing
 * typed by hand. This object only PARSES and VALIDATES; [PairingWriter.apply] writes.
 *
 * The rules, each from that document:
 *
 *  * a single JSON object; `type` exactly `promptshipper-pairing`, so an
 *    unrelated QR code is refused with a clear message instead of a parse error;
 *  * `v` exactly the integer `1`. **A different `v` is rejected** with a
 *    message telling the user to update the app — never parsed best-effort;
 *  * `stt` and `clip` each an object with `url` and `token`. A missing one is
 *    malformed: the app must not pair half a server silently;
 *  * each `url` passes [BaseUrlRule] — `http`/`https`, a host and port, no
 *    path — which also applies the private-range cleartext rule
 *    ([PrivateNetwork]);
 *  * each `token` is 24+ characters of `[A-Za-z0-9._~-]`, taken verbatim;
 *  * `name` is optional and display-only; unknown fields are ignored.
 *
 * 🔴 The payload holds two bearer tokens. Nothing here logs it, and no
 * rejection message quotes a token or the raw text.
 */
object PairingPayload {

    const val VERSION: Int = 1
    const val TYPE: String = "promptshipper-pairing"

    /** Generous: a v1 payload is ~280 bytes. Bounds work done on a hostile paste. */
    const val MAX_CHARS: Int = 8_192

    private val TOKEN = Regex("""^[A-Za-z0-9._~-]{24,1024}$""")

    data class Server(val url: String, val token: String) {
        /** Never the token: this is what a log line or a test failure shows. */
        override fun toString(): String = "Server(url=$url, token=<${token.length} chars>)"
    }

    data class Pairing(val name: String?, val stt: Server, val clip: Server) {
        /** What the confirmation screen calls this server. */
        val displayName: String get() = name ?: (PrivateNetwork.hostOf(stt.url) ?: stt.url)
    }

    sealed class Result {
        data class Ok(val pairing: Pairing) : Result()
        data class Rejected(val reason: String) : Result()
    }

    fun parse(text: String): Result {
        val trimmed = text.trim()
        if (trimmed.isEmpty()) return Result.Rejected("it is empty")
        if (trimmed.length > MAX_CHARS) return Result.Rejected("it is too long to be a pairing code")
        if (!trimmed.startsWith("{")) return Result.Rejected(NOT_OURS)
        val obj = try {
            // JSONTokener + an explicit end check: JSONObject(String) stops at
            // the closing brace and ignores whatever follows it.
            val tok = JSONTokener(trimmed)
            val value = tok.nextValue()
            if (tok.more()) return Result.Rejected("it has text after the JSON object")
            value as? JSONObject ?: return Result.Rejected(NOT_OURS)
        } catch (_: JSONException) {
            return Result.Rejected("it is not valid JSON")
        }

        if (obj.opt("type") != TYPE) return Result.Rejected(NOT_OURS)

        val v = obj.opt("v")
        val version = when (v) {
            is Int -> v
            is Long -> if (v in Int.MIN_VALUE..Int.MAX_VALUE) v.toInt() else null
            else -> null
        } ?: return Result.Rejected("its version field \"v\" is missing or not a whole number")
        if (version != VERSION) {
            return Result.Rejected(
                "it is a version $version pairing code and this app reads version " +
                    "$VERSION — update promptshipper"
            )
        }

        val name = when (val n = obj.opt("name")) {
            null, JSONObject.NULL -> null
            is String -> n.trim().takeIf { it.isNotEmpty() }?.take(63)
            else -> return Result.Rejected("its \"name\" is not text")
        }

        val stt = server(obj, "stt", SttEndpoints.TRANSCRIPTIONS_PATH)
        if (stt is Field.Bad) return Result.Rejected(stt.reason)
        val clip = server(obj, "clip", ClipEndpoints.CLIPS_PATH)
        if (clip is Field.Bad) return Result.Rejected(clip.reason)

        return Result.Ok(
            Pairing(
                name = name,
                stt = (stt as Field.Good).server,
                clip = (clip as Field.Good).server,
            )
        )
    }

    private sealed class Field {
        data class Good(val server: Server) : Field()
        data class Bad(val reason: String) : Field()
    }

    private fun server(obj: JSONObject, key: String, path: String): Field {
        val o = obj.opt(key) as? JSONObject
            ?: return Field.Bad("it has no \"$key\" server")
        val url = o.opt("url") as? String
            ?: return Field.Bad("the \"$key\" server has no url")
        val token = o.opt("token") as? String
            ?: return Field.Bad("the \"$key\" server has no token")
        if (url.isBlank()) return Field.Bad("the \"$key\" server has no url")
        BaseUrlRule.rejectionFor(url, path)?.let {
            return Field.Bad("the \"$key\" url is not usable: $it")
        }
        // Verbatim: "the exact string to send after `Bearer `" — no trimming.
        if (!TOKEN.matches(token)) {
            return Field.Bad(
                "the \"$key\" token is not a valid token (24 or more letters, " +
                    "digits or . _ ~ -)"
            )
        }
        return Field.Good(Server(url.trim().trimEnd('/'), token))
    }

    private const val NOT_OURS = "it is not a promptshipper pairing code"
}

/**
 * Writes a validated [PairingPayload.Pairing] into the two settings objects.
 *
 * 🔴 All or nothing. Four values are written — two URLs, two encrypted tokens
 * — and an encryption can throw ([TokenStore.putToken] propagates the cipher's
 * failure on purpose). A half-applied pairing would leave the transcription
 * half pointing at the new server and the clip half at the old one, or a new
 * URL with an old token: every request a 401 with no explanation. So the
 * previous values are read first and put back if any write fails.
 */
object PairingWriter {

    fun apply(pairing: PairingPayload.Pairing, stt: SttSettings, clip: ClipSettings) {
        val before = Snapshot(
            sttUrl = stt.baseUrl,
            sttToken = stt.tokens.ciphertext(),
            sttRemote = stt.remoteEnabled,
            clipUrl = clip.baseUrl,
            clipToken = clip.tokens.ciphertext(),
        )
        try {
            stt.tokens.putToken(pairing.stt.token)
            clip.tokens.putToken(pairing.clip.token)
            stt.baseUrl = pairing.stt.url
            clip.baseUrl = pairing.clip.url
            stt.remoteEnabled = true
        } catch (e: RuntimeException) {
            restore(before, stt, clip)
            throw e
        }
    }

    /** Tokens as CIPHERTEXT: restoring them must not need the cipher. */
    private data class Snapshot(
        val sttUrl: String?,
        val sttToken: String?,
        val sttRemote: Boolean,
        val clipUrl: String?,
        val clipToken: String?,
    )

    private fun restore(s: Snapshot, stt: SttSettings, clip: ClipSettings) {
        stt.tokens.restoreCiphertext(s.sttToken)
        clip.tokens.restoreCiphertext(s.clipToken)
        stt.baseUrl = s.sttUrl
        clip.baseUrl = s.clipUrl
        stt.remoteEnabled = s.sttRemote
    }

    /** Whether the app knows a transcription server at all — the first-run test. */
    fun isPaired(stt: SttSettings): Boolean = stt.baseUrl != null
}
