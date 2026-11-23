package dev.zacx.promptshipper

import java.io.IOException
import java.net.HttpURLConnection
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors

/**
 * Fetches the snippet catalog from the paired STT server's `GET /v1/snippets`
 * and stores it through [SnipStore.save], the same write the file import uses.
 *
 * ## 🔴 A failure NEVER touches the stored catalog
 *
 * [SnipStore.save] is reached on exactly one path: a 200 whose body
 * [SnipCatalog.parse] accepted and which holds at least one row. Every other
 * answer (no pairing, no token, a transport fault, 401/403, 503, any other
 * status, a body that is not a catalog) returns its own [Outcome] and writes
 * nothing, so a phone with a working catalog keeps it through a server outage
 * or a rotated token.
 *
 * A 200 carrying `[]` is refused too. The server never sends one (it answers
 * 503 for an empty catalog, for the reason it gives in `server/stt/app.py`: an
 * empty answer would be read as "you have no snippets" and wipe the client's
 * copy). Accepting it here would make that wipe possible from any other server
 * that answers the route.
 *
 * ## Conditional GET
 *
 * The stored [SnipStore.Catalog.etag] goes out as `If-None-Match`; a 304 is
 * [Outcome.Unchanged] and writes nothing. The ETag lives in the catalog's own
 * envelope, so a file import (which stores none) forces the next sync to
 * download again.
 *
 * `HttpURLConnection` through [PrivateNetwork.open], for [SttHttp]'s reasons.
 * Blocking: call it off the main thread ([launch]).
 */
class SnipSync(
    private val store: SnipStore,
    /** Read at sync time, so a sync built before a pairing uses the new values. */
    private val credentials: () -> Credentials?,
    /** Injected for the test; the production value opens a real connection. */
    private val opener: (String) -> HttpURLConnection = PrivateNetwork::open,
    private val clock: () -> Long = System::currentTimeMillis,
) {

    data class Credentials(val baseUrl: String, val token: String) {
        override fun toString(): String = "Credentials(baseUrl=$baseUrl, token=<${token.length} chars>)"
    }

    /** What one sync did. Every case but [Synced] left the stored catalog alone. */
    sealed class Outcome {
        /** A new catalog of [count] rows was stored. */
        data class Synced(val count: Int) : Outcome()

        /** 304: the stored catalog is what the server holds. */
        object Unchanged : Outcome()

        /** No server URL, or no usable token, so nothing was requested. */
        object NotPaired : Outcome()

        /** 401 or 403: the server refused the token. */
        data class Unauthorized(val status: Int) : Outcome()

        /** 503: the server has no usable catalog; [detail] is its reason. */
        data class Unavailable(val detail: String?) : Outcome()

        /** Any other status. */
        data class HttpError(val status: Int) : Outcome()

        /** A 200 whose body is not a usable catalog; [reason] says why. */
        data class Invalid(val reason: String) : Outcome()

        /** No response at all: refused, timed out, unreachable. */
        data class Network(val message: String) : Outcome()

        /** The catalog arrived and parsed, but writing it failed. */
        data class StoreFailed(val message: String) : Outcome()
    }

    /** One blocking sync. Never throws. */
    fun sync(): Outcome {
        val creds = runCatching { credentials() }.getOrNull() ?: return Outcome.NotPaired
        val previousEtag = runCatching { store.catalog()?.etag }.getOrNull()
        var conn: HttpURLConnection? = null
        return try {
            conn = opener(SttEndpoints.snippets(creds.baseUrl)).apply {
                requestMethod = "GET"
                connectTimeout = SttRetryPolicy.CONNECT_TIMEOUT_MS
                readTimeout = READ_TIMEOUT_MS
                setRequestProperty("Authorization", SttEndpoints.authorization(creds.token))
                setRequestProperty("Accept", "application/json")
                if (previousEtag != null) setRequestProperty("If-None-Match", previousEtag)
            }
            when (val status = conn.responseCode) {
                HttpURLConnection.HTTP_OK -> accept(conn)
                HttpURLConnection.HTTP_NOT_MODIFIED -> Outcome.Unchanged
                HttpURLConnection.HTTP_UNAUTHORIZED, HttpURLConnection.HTTP_FORBIDDEN ->
                    Outcome.Unauthorized(status)
                HttpURLConnection.HTTP_UNAVAILABLE -> Outcome.Unavailable(
                    conn.errorStream?.use { it.readBytes().toString(Charsets.UTF_8) }
                        ?.let { JsonLite.stringField(it, "detail") }
                )
                else -> Outcome.HttpError(status)
            }
        } catch (e: IOException) {
            // e.message only, for SttHttp's reason.
            Outcome.Network(e.message ?: e.javaClass.simpleName)
        } catch (e: RuntimeException) {
            Outcome.Network(e.message ?: e.javaClass.simpleName)
        } finally {
            conn?.disconnect()
        }
    }

    private fun accept(conn: HttpURLConnection): Outcome {
        val body = conn.inputStream.use { it.readBytes().toString(Charsets.UTF_8) }
        val rows = try {
            SnipCatalog.parse(body)
        } catch (e: SnipCatalog.Invalid) {
            return Outcome.Invalid(e.reason)
        }
        if (rows.isEmpty()) return Outcome.Invalid(EMPTY_REASON)
        return try {
            store.save(rows, clock(), fromServer = true, etag = conn.getHeaderField("ETag"))
            Outcome.Synced(rows.size)
        } catch (e: IOException) {
            Outcome.StoreFailed(e.message ?: e.javaClass.simpleName)
        }
    }

    companion object {
        private const val READ_TIMEOUT_MS = 15_000

        /** [Outcome.Invalid]'s reason for a 200 carrying zero rows. */
        const val EMPTY_REASON: String = "the server sent an empty catalog"

        /**
         * The STT server and token from [stt], or null when either is
         * missing. The token is the transcription token: `/v1/snippets` is
         * served by the same server behind the same bearer check.
         */
        fun credentialsOf(stt: SttSettings): () -> Credentials? = {
            val url = stt.baseUrl
            val token = stt.tokens.token()
            if (url != null && token != null) Credentials(url, token) else null
        }

        /** A sync of [stt]'s server into the app's own [SnipStore]. */
        fun forServer(context: android.content.Context, stt: SttSettings): SnipSync =
            SnipSync(SnipStore(context.applicationContext), credentialsOf(stt))

        /**
         * One worker for every background sync in the process, so a pairing
         * and a settings screen opening at the same moment run one after the
         * other rather than racing two downloads.
         */
        private val executor: ExecutorService = Executors.newSingleThreadExecutor { r ->
            Thread(r, "snip-sync").apply { isDaemon = true }
        }

        /** Runs [sync] off the main thread; [then] gets the outcome on the worker. */
        fun launch(sync: SnipSync, then: (Outcome) -> Unit = {}) {
            executor.execute { then(sync.sync()) }
        }
    }
}
