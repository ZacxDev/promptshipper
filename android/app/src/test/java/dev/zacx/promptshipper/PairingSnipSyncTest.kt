package dev.zacx.promptshipper

import android.content.Context
import android.widget.Button
import android.widget.EditText
import dev.zacx.promptshipper.SnipSyncFixtures.BODY
import dev.zacx.promptshipper.SnipSyncFixtures.ETAG
import dev.zacx.promptshipper.SnipSyncFixtures.LABELS
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import java.io.File

/**
 * Applying a pairing fetches the new server's snippet catalog.
 *
 * End to end on purpose: a real loopback HTTP server, the production opener
 * ([PrivateNetwork.open]), the production [SnipStore] directory, and the real
 * Pair button. Only the two settings objects are injected, with an identity
 * cipher, because Robolectric has no AndroidKeyStore (see
 * [PairingActivityTest]'s kdoc), so without them a pairing can never succeed
 * here.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class PairingSnipSyncTest {

    private val ctx get() = RuntimeEnvironment.getApplication()

    private val storeDir get() = File(ctx.noBackupFilesDir, SnipStore.DIR)

    private object Identity : SecretCipher {
        override fun encrypt(plaintext: String) = plaintext
        override fun decrypt(blob: String): String? = blob
    }

    @Before
    fun clean() {
        storeDir.deleteRecursively()
    }

    @After
    fun cleanAfter() {
        storeDir.deleteRecursively()
    }

    @Test
    fun applyingAPairingFetchesTheSnippetCatalogWithTheNewToken() {
        val token = "Pq4".repeat(10)
        LoopbackHttpServer(200, "OK", BODY, mapOf("ETag" to ETAG)).use { server ->
            val payload =
                """{"v":1,"type":"promptshipper-pairing","name":"box",""" +
                    """"stt":{"url":"${server.baseUrl}","token":"$token"},""" +
                    """"clip":{"url":"http://100.101.102.103:8139","token":"${"b".repeat(64)}"}}"""

            val p = ctx.getSharedPreferences("pairing_snip_sync_test", Context.MODE_PRIVATE)
            val c = Robolectric.buildActivity(PairingActivity::class.java)
            c.get().sttOverride = SttSettings(p, TokenStore(p, Identity))
            c.get().clipOverride = ClipSettings(p, TokenStore(p, Identity, ClipSettings.KEY_TOKEN_CIPHERTEXT))
            val a = c.setup().get()

            a.findViewById<EditText>(R.id.pairing_input).setText(payload)
            val pair = a.findViewById<Button>(R.id.pair_confirm)
            assertTrue("the payload was not accepted, so Pair is disabled", pair.isEnabled)
            pair.performClick()
            assertTrue("the pairing did not apply", a.isFinishing)

            val request = server.nextRequest()
            assertNotNull("applying a pairing sent no request to the STT server", request)
            assertEquals("GET /v1/snippets HTTP/1.1", request!![0])
            assertEquals("Bearer $token", LoopbackHttpServer.header(request, "Authorization"))

            // And the answer landed in the store the picker reads.
            val store = SnipStore(ctx)
            val deadline = System.currentTimeMillis() + 5_000
            while (store.catalog() == null && System.currentTimeMillis() < deadline) Thread.sleep(20)
            assertEquals(LABELS, store.catalog()?.snippets?.map { it.label })
            c.pause().stop().destroy()
        }
    }
}
