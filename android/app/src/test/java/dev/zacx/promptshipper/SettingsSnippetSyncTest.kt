package dev.zacx.promptshipper

import android.os.Looper
import android.widget.Button
import android.widget.TextView
import dev.zacx.promptshipper.SnipSyncFixtures.BASE_URL
import dev.zacx.promptshipper.SnipSyncFixtures.BODY
import dev.zacx.promptshipper.SnipSyncFixtures.CLOCK_MS
import dev.zacx.promptshipper.SnipSyncFixtures.ETAG
import dev.zacx.promptshipper.SnipSyncFixtures.PREVIOUS
import dev.zacx.promptshipper.SnipSyncFixtures.TOKEN
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.android.controller.ActivityController
import org.robolectric.annotation.Config
import java.io.File

/**
 * The Settings side of [SnipSync]: the "Sync from server" button, the sync on
 * open, and the words on the snippets line.
 *
 * The sync is injected with a scripted connection because Robolectric has no
 * AndroidKeyStore, so a real stored token cannot be decrypted here. The
 * expected lines are literal English (`en-rUS`), not re-derived from the
 * resources.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34], qualifiers = "en-rUS")
class SettingsSnippetSyncTest {

    @get:Rule
    val temp = TemporaryFolder()

    private var controller: ActivityController<SettingsActivity>? = null

    @After
    fun tearDown() {
        controller?.pause()?.stop()?.destroy()
    }

    private fun start(store: SnipStore, opener: ScriptedOpener): SettingsActivity {
        val c = Robolectric.buildActivity(SettingsActivity::class.java)
        controller = c
        c.get().snippets = store
        c.get().snipSync = SnipSync(store, { SnipSync.Credentials(BASE_URL, TOKEN) }, opener, clock = { CLOCK_MS })
        return c.setup().get()
    }

    private fun line(a: SettingsActivity) =
        a.findViewById<TextView>(R.id.stt_snippets_state).text?.toString().orEmpty()

    /** Drains the main looper until [done] holds; returns the last line read. */
    private fun awaitLine(a: SettingsActivity, done: (String) -> Boolean): String {
        val deadline = System.currentTimeMillis() + 5_000
        var now = line(a)
        while (!done(now) && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            shadowOf(Looper.getMainLooper()).idle()
            now = line(a)
        }
        return now
    }

    private fun tapSync(a: SettingsActivity) {
        a.findViewById<Button>(R.id.stt_sync_snippets).performClick()
    }

    @Test
    fun theSyncButtonShowsTheSyncedCountAndSaysItCameFromTheServer() {
        val store = SnipStore(temp.newFolder("store"))
        val opener = ScriptedOpener(200, BODY, mapOf("ETag" to ETAG))
        val a = start(store, opener)
        awaitLine(a) { it == "No snippets imported yet" }

        tapSync(a)
        val shown = awaitLine(a) { it.startsWith("3 snippets") }
        assertTrue("the line after a sync is: $shown", shown.startsWith("3 snippets, synced from server "))
        assertEquals(1, opener.calls.get())
    }

    @Test
    fun aRefusedTokenIsNamedAndTheCatalogStillOnThePhoneIsShown() {
        val store = SnipStore(temp.newFolder("store")).apply { save(PREVIOUS, importedAtMs = CLOCK_MS) }
        val before = File(store.dir, SnipStore.FILE).readBytes()
        val a = start(store, ScriptedOpener(401, """{"detail":"bad token"}"""))
        awaitLine(a) { it.startsWith("2 snippets") }

        tapSync(a)
        val shown = awaitLine(a) { it.startsWith("Sync failed") }
        assertTrue(
            "the line after a 401 is: $shown",
            shown.startsWith(
                "Sync failed: the server refused the token (HTTP 401). Re-pair this phone.\n" +
                    "2 snippets, imported ",
            ),
        )
        assertArrayEquals(before, File(store.dir, SnipStore.FILE).readBytes())
    }

    @Test
    fun aServerWithNoCatalogShowsItsReason() {
        val store = SnipStore(temp.newFolder("store"))
        val a = start(store, ScriptedOpener(503, """{"detail":"no snip catalog is mounted"}"""))
        awaitLine(a) { it == "No snippets imported yet" }

        tapSync(a)
        assertEquals(
            "Sync failed: the server has no usable snippet catalog (no snip catalog is mounted).\n" +
                "No snippets imported yet",
            awaitLine(a) { it.startsWith("Sync failed") },
        )
    }

    @Test
    fun openingSettingsOnAPairedPhoneSyncs() {
        SttSettings.prefs(RuntimeEnvironment.getApplication()).edit()
            .putString(SttSettings.KEY_BASE_URL, BASE_URL).commit()
        val opener = ScriptedOpener(200, BODY, mapOf("ETag" to ETAG))
        val a = start(SnipStore(temp.newFolder("store")), opener)

        val shown = awaitLine(a) { it.startsWith("3 snippets") }
        assertTrue("the line after opening is: $shown", shown.startsWith("3 snippets, synced from server "))
        assertEquals(1, opener.calls.get())
    }

    @Test
    fun openingSettingsUnpairedDoesNotSync() {
        val opener = ScriptedOpener(200, BODY)
        val a = start(SnipStore(temp.newFolder("store")), opener)
        assertEquals("No snippets imported yet", awaitLine(a) { it == "No snippets imported yet" })
        // Let anything else queued on the worker land too.
        Thread.sleep(200)
        shadowOf(Looper.getMainLooper()).idle()
        assertEquals("an unpaired Settings screen fetched the catalog", 0, opener.calls.get())
        assertEquals("No snippets imported yet", line(a))
    }
}
