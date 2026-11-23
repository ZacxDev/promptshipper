package dev.zacx.promptshipper

import dev.zacx.promptshipper.SnipSyncFixtures.BASE_URL
import dev.zacx.promptshipper.SnipSyncFixtures.BODY
import dev.zacx.promptshipper.SnipSyncFixtures.CLOCK_MS
import dev.zacx.promptshipper.SnipSyncFixtures.ETAG
import dev.zacx.promptshipper.SnipSyncFixtures.LABELS
import dev.zacx.promptshipper.SnipSyncFixtures.PREVIOUS
import dev.zacx.promptshipper.SnipSyncFixtures.PREVIOUS_ETAG
import dev.zacx.promptshipper.SnipSyncFixtures.TOKEN
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File
import java.net.InetAddress
import java.net.ServerSocket

/**
 * [SnipSync]: `GET /v1/snippets` into [SnipStore].
 *
 * The failure cases are the point of this file. Each one starts from a stored
 * catalog ([PREVIOUS]) and asserts the catalog FILE is byte-identical after
 * the failed sync, plus the literal [SnipSync.Outcome] it produced.
 *
 * ⚠ Those byte-identical assertions are INVARIANT GUARDS: there was no fetch
 * before this change, so they cannot be red at the base commit. Their red was
 * shown by mutation instead (a 503 that calls `save(emptyList())`); see the PR.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class SnipSyncTest {

    @get:Rule
    val temp = TemporaryFolder()

    private var counter = 0

    private fun store() = SnipStore(temp.newFolder("snip-sync-${counter++}"))

    /** A store already holding [PREVIOUS], as a phone that synced once would. */
    private fun seeded(): SnipStore = store().apply {
        save(PREVIOUS, importedAtMs = 1_700_000_123_000L, fromServer = true, etag = PREVIOUS_ETAG)
    }

    private fun bytes(store: SnipStore): ByteArray = File(store.dir, SnipStore.FILE).readBytes()

    private fun sync(store: SnipStore, opener: (String) -> java.net.HttpURLConnection) =
        SnipSync(store, { SnipSync.Credentials(BASE_URL, TOKEN) }, opener, clock = { CLOCK_MS }).sync()

    /** Runs a failing answer over [seeded] and asserts the file did not move. */
    private fun failsWithoutWiping(opener: (String) -> java.net.HttpURLConnection): SnipSync.Outcome {
        val s = seeded()
        val before = bytes(s)
        val outcome = sync(s, opener)
        assertArrayEquals(
            "a failed sync ($outcome) rewrote the stored catalog; a failure must " +
                "leave what the phone already has",
            before,
            bytes(s),
        )
        assertEquals(PREVIOUS.map { it.label }, s.catalog()!!.snippets.map { it.label })
        return outcome
    }

    // --- 200 -----------------------------------------------------------------

    @Test
    fun aTwoHundredStoresTheServersCatalogWithItsEtag() {
        val s = store()
        val outcome = sync(s, ScriptedOpener(200, BODY, mapOf("ETag" to ETAG)))

        assertEquals(SnipSync.Outcome.Synced(3), outcome)
        val stored = s.catalog()
        assertNotNull(stored)
        assertEquals(LABELS, stored!!.snippets.map { it.label })
        assertEquals(3, stored.snippets.size)
        assertEquals("pulp, then seeds", stored.snippets[1].body)
        assertEquals(CLOCK_MS, stored.importedAtMs)
        assertTrue("a synced catalog is not marked as from the server", stored.fromServer)
        assertEquals(ETAG, stored.etag)
    }

    @Test
    fun aTwoHundredReplacesAPreviousCatalog() {
        val s = seeded()
        assertEquals(SnipSync.Outcome.Synced(3), sync(s, ScriptedOpener(200, BODY, mapOf("ETag" to ETAG))))
        assertEquals(LABELS, s.catalog()!!.snippets.map { it.label })
    }

    // --- the request ---------------------------------------------------------

    @Test
    fun theRequestIsAGetOfTheSnippetsEndpointWithTheBearerToken() {
        val opener = ScriptedOpener(200, BODY)
        sync(store(), opener)
        val conn = opener.last!!
        assertEquals("http://100.101.102.103:8118/v1/snippets", conn.openedUrl)
        assertEquals("GET", conn.requestMethod)
        assertEquals("Bearer Zq7-snipsync-test-token-4c19e2b6a0d8f357", conn.headers["Authorization"])
    }

    @Test
    fun theBearerTokenGoesOverARealSocketThroughTheProductionOpener() {
        LoopbackHttpServer(200, "OK", BODY, mapOf("ETag" to ETAG)).use { server ->
            val s = store()
            val outcome = SnipSync(s, { SnipSync.Credentials(server.baseUrl, TOKEN) }, clock = { CLOCK_MS }).sync()
            val request = server.nextRequest()
            assertNotNull("the server saw no request", request)
            assertEquals("GET /v1/snippets HTTP/1.1", request!![0])
            assertEquals(
                "Bearer Zq7-snipsync-test-token-4c19e2b6a0d8f357",
                LoopbackHttpServer.header(request, "Authorization"),
            )
            assertEquals(SnipSync.Outcome.Synced(3), outcome)
            assertEquals(LABELS, s.catalog()!!.snippets.map { it.label })
        }
    }

    // --- 304 -----------------------------------------------------------------

    @Test
    fun aThreeOhFourKeepsTheCatalogAndTheStoredEtagWasSent() {
        val opener = ScriptedOpener(304)
        val outcome = failsWithoutWiping(opener)
        assertEquals(SnipSync.Outcome.Unchanged, outcome)
        assertEquals("\"4410-heron-juniper\"", opener.last!!.headers["If-None-Match"])
    }

    @Test
    fun aFileImportedCatalogSendsNoIfNoneMatch() {
        // A file import stores no ETag, so the server must be asked for the
        // whole catalog rather than told "I already have yours".
        val s = store().apply { save(PREVIOUS, importedAtMs = 5L) }
        val opener = ScriptedOpener(200, BODY)
        sync(s, opener)
        assertFalse(
            "If-None-Match was sent for a catalog the server never sent",
            opener.last!!.headers.containsKey("If-None-Match"),
        )
    }

    // --- failures: each keeps the catalog, each its own outcome --------------

    @Test
    fun aFourOhOneKeepsTheCatalog() {
        assertEquals(SnipSync.Outcome.Unauthorized(401), failsWithoutWiping(ScriptedOpener(401, """{"detail":"bad token"}""")))
    }

    @Test
    fun aFourOhThreeKeepsTheCatalog() {
        assertEquals(SnipSync.Outcome.Unauthorized(403), failsWithoutWiping(ScriptedOpener(403, "")))
    }

    @Test
    fun aFiveOhThreeKeepsTheCatalogAndCarriesTheServersReason() {
        assertEquals(
            SnipSync.Outcome.Unavailable("the snip catalog is not a JSON array of snippet objects"),
            failsWithoutWiping(
                ScriptedOpener(503, """{"detail":"the snip catalog is not a JSON array of snippet objects"}"""),
            ),
        )
    }

    @Test
    fun aGarbageBodyKeepsTheCatalog() {
        assertEquals(
            SnipSync.Outcome.Invalid("the file is not a JSON array of snippets"),
            failsWithoutWiping(ScriptedOpener(200, "<html>proxy error</html>")),
        )
    }

    @Test
    fun aRowWithNoLabelKeepsTheCatalog() {
        assertEquals(
            SnipSync.Outcome.Invalid("entry 2 has no \"label\""),
            failsWithoutWiping(ScriptedOpener(200, """[{"label":"Okapi ledger"},{"body":"x"}]""")),
        )
    }

    @Test
    fun anEmptyArrayKeepsTheCatalog() {
        assertEquals(
            SnipSync.Outcome.Invalid("the server sent an empty catalog"),
            failsWithoutWiping(ScriptedOpener(200, "[]")),
        )
    }

    @Test
    fun anotherStatusKeepsTheCatalog() {
        assertEquals(SnipSync.Outcome.HttpError(500), failsWithoutWiping(ScriptedOpener(500, "oops")))
    }

    @Test
    fun aRefusedConnectionKeepsTheCatalog() {
        // A real refused connect: a port that was bound and released, through
        // the production opener.
        val port = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1")).use { it.localPort }
        val s = seeded()
        val before = bytes(s)
        val outcome = SnipSync(s, { SnipSync.Credentials("http://127.0.0.1:$port", TOKEN) }).sync()
        assertTrue("a refused connect is not a Network outcome: $outcome", outcome is SnipSync.Outcome.Network)
        assertArrayEquals(before, bytes(s))
    }

    @Test
    fun noPairingMakesNoRequestAndKeepsTheCatalog() {
        val s = seeded()
        val before = bytes(s)
        val opener = ScriptedOpener(200, BODY)
        assertEquals(SnipSync.Outcome.NotPaired, SnipSync(s, { null }, opener).sync())
        assertEquals("an unpaired sync opened a connection", 0, opener.calls.get())
        assertArrayEquals(before, bytes(s))
    }

    @Test
    fun noTokenInTheSettingsIsNotPaired() {
        val p = org.robolectric.RuntimeEnvironment.getApplication()
            .getSharedPreferences("snip_sync_test", android.content.Context.MODE_PRIVATE)
        val stt = SttSettings(p, TokenStore(p, object : SecretCipher {
            override fun encrypt(plaintext: String) = plaintext
            override fun decrypt(blob: String): String? = blob
        }))
        stt.baseUrl = BASE_URL
        assertNull(SnipSync.credentialsOf(stt)())
        stt.tokens.putToken(TOKEN)
        assertEquals(SnipSync.Credentials(BASE_URL, TOKEN), SnipSync.credentialsOf(stt)())
    }

    @Test
    fun everyFailureIsADistinctOutcome() {
        val port = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1")).use { it.localPort }
        val outcomes = listOf(
            SnipSync(store(), { null }).sync(),
            sync(seeded(), ScriptedOpener(401)),
            sync(seeded(), ScriptedOpener(503, """{"detail":"no snip catalog is mounted"}""")),
            sync(seeded(), ScriptedOpener(200, "not json")),
            sync(seeded(), ScriptedOpener(500)),
            SnipSync(seeded(), { SnipSync.Credentials("http://127.0.0.1:$port", TOKEN) }).sync(),
            sync(seeded(), ScriptedOpener(304)),
        )
        assertEquals(
            "two different failures produced the same kind of outcome, so the " +
                "screen could not name them apart: $outcomes",
            outcomes.size,
            outcomes.map { it.javaClass }.toSet().size,
        )
    }

    @Test
    fun theCatalogEnvelopeRoundTripsSourceAndEtag() {
        val s = store()
        s.save(PREVIOUS, importedAtMs = 9L, fromServer = true, etag = PREVIOUS_ETAG)
        assertEquals(SnipStore.Catalog(PREVIOUS, 9L, fromServer = true, etag = PREVIOUS_ETAG), s.catalog())
        s.save(PREVIOUS, importedAtMs = 10L)
        assertEquals(SnipStore.Catalog(PREVIOUS, 10L, fromServer = false, etag = null), s.catalog())
    }
}
