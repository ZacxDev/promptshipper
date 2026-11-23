package dev.zacx.promptshipper

import java.io.ByteArrayInputStream
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.InetAddress
import java.net.ServerSocket
import java.net.URL
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Fixtures and fakes shared by the snippet-sync tests.
 *
 * 🔴 The fixture's labels and its count (3) are chosen so that nothing the
 * production code names can produce them: no label is a string constant in
 * main sources, and 3 is not 0, 1 or any status code.
 */
object SnipSyncFixtures {
    const val BASE_URL = "http://100.101.102.103:8118"
    const val TOKEN = "Zq7-snipsync-test-token-4c19e2b6a0d8f357"
    const val ETAG = "\"9b1e-okapi-tamarind-vellum\""
    const val CLOCK_MS = 1_765_432_100_000L

    val LABELS = listOf("Okapi ledger", "Tamarind sprint", "Vellum route")

    val BODY = """
        [
          {"label":"Okapi ledger","body":"count the stripes","search_terms":["zoo"],"category":"prompt"},
          {"label":"Tamarind sprint","body":"pulp, then seeds","search_terms":[],"category":"path"},
          {"label":"Vellum route","body":"north by the mill","category":"prompt"}
        ]
    """.trimIndent()

    /** What is on the phone before a sync: two rows that are NOT in [BODY]. */
    val PREVIOUS = listOf(
        Snippet("Heron bypass", "left at the weir", listOf("bird"), "path"),
        Snippet("Juniper atlas", "page forty", emptyList(), "prompt"),
    )
    const val PREVIOUS_ETAG = "\"4410-heron-juniper\""
}

/**
 * A scripted `HttpURLConnection` for [SnipSync]'s injected opener, in
 * [SttHttpTest]'s FakeConnection shape: it records what was set on it and
 * answers [status], [body] and [responseHeaders].
 */
class FakeSnipConnection(
    url: String,
    private val status: Int,
    private val body: String,
    private val responseHeaders: Map<String, String> = emptyMap(),
) : HttpURLConnection(URL(url)) {

    val headers = mutableMapOf<String, String>()
    val openedUrl: String = url

    override fun setRequestProperty(key: String, value: String?) {
        headers[key] = value.orEmpty()
    }

    override fun getResponseCode(): Int = status

    override fun getInputStream(): InputStream {
        if (status >= 400) throw IOException("no input stream on an error")
        return ByteArrayInputStream(body.toByteArray())
    }

    override fun getErrorStream(): InputStream? =
        if (status >= 400) ByteArrayInputStream(body.toByteArray()) else null

    override fun getHeaderField(name: String): String? =
        responseHeaders.entries.firstOrNull { it.key.equals(name, ignoreCase = true) }?.value

    override fun disconnect() {}
    override fun usingProxy(): Boolean = false
    override fun connect() {}
}

/** An opener that answers every request with one scripted response, and counts. */
class ScriptedOpener(
    private val status: Int,
    private val body: String = "",
    private val responseHeaders: Map<String, String> = emptyMap(),
) : (String) -> HttpURLConnection {
    val calls = AtomicInteger(0)
    @Volatile var last: FakeSnipConnection? = null

    override fun invoke(url: String): HttpURLConnection {
        calls.incrementAndGet()
        return FakeSnipConnection(url, status, body, responseHeaders).also { last = it }
    }
}

/**
 * A real HTTP/1.1 server on 127.0.0.1 that answers every request with one
 * fixed response. Used where the test must go through the production opener
 * ([PrivateNetwork.open]) and a real socket, not a fake connection.
 */
class LoopbackHttpServer(status: Int, reason: String, body: String, headers: Map<String, String> = emptyMap()) :
    AutoCloseable {

    private val socket = ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"))
    private val response: ByteArray = buildString {
        append("HTTP/1.1 $status $reason\r\n")
        append("Content-Type: application/json\r\n")
        for ((k, v) in headers) append("$k: $v\r\n")
        append("Content-Length: ${body.toByteArray().size}\r\n")
        append("Connection: close\r\n\r\n")
        append(body)
    }.toByteArray()

    /** Header lines of each request received, request line first. */
    val requests = LinkedBlockingQueue<List<String>>()

    val baseUrl: String get() = "http://127.0.0.1:${socket.localPort}"

    init {
        Thread {
            while (!socket.isClosed) {
                try {
                    socket.accept().use { c ->
                        val reader = c.getInputStream().bufferedReader(Charsets.ISO_8859_1)
                        val lines = mutableListOf<String>()
                        while (true) {
                            val line = reader.readLine() ?: break
                            if (line.isEmpty()) break
                            lines += line
                        }
                        requests.put(lines)
                        c.getOutputStream().apply { write(response); flush() }
                    }
                } catch (_: IOException) {
                    // closed, or a client that hung up
                }
            }
        }.apply { isDaemon = true; start() }
    }

    fun nextRequest(seconds: Long = 10): List<String>? = requests.poll(seconds, TimeUnit.SECONDS)

    override fun close() = socket.close()

    companion object {
        fun header(request: List<String>, name: String): String? =
            request.drop(1).firstOrNull { it.substringBefore(':').trim().equals(name, ignoreCase = true) }
                ?.substringAfter(':')?.trim()
    }
}
