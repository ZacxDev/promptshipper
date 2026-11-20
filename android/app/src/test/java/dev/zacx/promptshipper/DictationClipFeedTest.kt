package dev.zacx.promptshipper

import android.content.Context
import android.os.Looper
import android.widget.Button
import android.widget.CheckBox
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import java.io.File
import java.util.Locale

/**
 * "Send each dictation to my clip feed" ([DictationClipFeed]): opt-in, default
 * off, one clip per finished transcript through the existing outbox.
 *
 * The session tests drive the REAL [DictationSession] entry point
 * (`MicListener.onFinished`, by reflection — the same mechanism as
 * [DictationSessionRemoteHistoryWiringTest]) with the real ladder over a
 * scripted transport, into a real [ClipOutboxStore] in a temp dir. The
 * setting is read from a real [ClipSettings], so "off" is the stored default
 * and not a test double's constant.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class DictationClipFeedTest {

    @get:Rule
    val temp = TemporaryFolder()

    private val ctx: Context get() = RuntimeEnvironment.getApplication()
    private var counter = 0

    private class Silent : DictationSession.Callbacks {
        override fun onTranscript(text: String, fromRemote: Boolean, isFinal: Boolean) = Unit
        override fun onStatus(text: String) = Unit
        override fun onLevel(level: Double) = Unit
        override fun onTier(tier: AudioCaptureLogic.Tier) = Unit
        override fun onClientSilenced(silenced: Boolean) = Unit
        override fun onRecordingStopped() = Unit
        override fun onHistorySaved(id: String) = Unit
    }

    private object Identity : SecretCipher {
        override fun encrypt(plaintext: String): String = plaintext
        override fun decrypt(blob: String): String? = blob
    }

    /** The production preferences file, so a restart reads what Settings wrote. */
    private fun clipSettings() = ClipSettings(
        SttSettings.prefs(ctx),
        TokenStore(SttSettings.prefs(ctx), Identity, ClipSettings.KEY_TOKEN_CIPHERTEXT),
    )

    private fun sttSettings(): SttSettings {
        val p = ctx.getSharedPreferences("feed-stt-${counter++}", Context.MODE_PRIVATE)
        return SttSettings(p, TokenStore(p, Identity).apply { putToken("a-bearer-token") })
            .apply { baseUrl = "http://100.101.102.103:8118" }
    }

    private fun transcriber(outcome: SttOutcome, uploads: MutableList<Int>):
        (Context, SttSettings, Locale) -> RemoteTranscriber = { _, _, _ ->
        RemoteTranscriber(
            ctx, sttSettings(), locale = Locale("en", "US"),
            transportFactory = { _, _ ->
                RemoteTranscriber.Transport(
                    upload = { uploads += 1; outcome },
                    health = { true },
                )
            },
            sleeper = { }, unitRandom = { 0.5 }, elapsedMs = { 0L },
        )
    }

    private fun dictate(
        outbox: ClipOutboxStore,
        outcome: SttOutcome,
        local: String = "the phone heard this",
    ): Int {
        val uploads = mutableListOf<Int>()
        val session = DictationSession(
            ctx, sttSettings(), Silent(),
            history = DictationHistoryStore(temp.newFolder("h${counter++}"), { 1_700_000_000_000L }),
            transcriberFactory = transcriber(outcome, uploads),
            clipFeed = DictationClipFeed(
                enabled = { clipSettings().feedDictations },
                outbox = { outbox },
            ),
        )
        DictationSession::class.java.getDeclaredField("localTranscript")
            .apply { isAccessible = true }.set(session, local)
        val file = File(temp.newFolder("src${counter++}"), "d.ogg").apply { writeBytes(ByteArray(4096)) }
        val listener = Class.forName("dev.zacx.promptshipper.DictationSession\$MicListener")
            .getDeclaredConstructor(DictationSession::class.java).apply { isAccessible = true }
            .newInstance(session) as MicCapture.Listener
        listener.onFinished(file, 3.0)
        Thread.sleep(300)
        shadowOf(Looper.getMainLooper()).idle()
        session.destroy()
        return uploads.size
    }

    private fun outbox() = ClipOutboxStore(temp.newFolder("outbox${counter++}"))

    @Test
    fun itIsOffByDefault() {
        assertFalse("the dictation feed must be opt-in", clipSettings().feedDictations)
    }

    @Test
    fun offQueuesNothing() {
        // 🔴 The privacy direction. Red if the gate in DictationClipFeed.offer
        // is removed: the transcript would be queued with the setting off.
        val box = outbox()
        val uploads = dictate(box, SttOutcome.Success("the server heard this"))
        assertEquals("positive control: the dictation really completed", 1, uploads)
        assertEquals("a dictation reached the clip queue with the feed OFF", emptyList<ClipOutbox.Pending>(), box.pending())
    }

    @Test
    fun onQueuesExactlyOneClipPerTranscriptTagged() {
        clipSettings().feedDictations = true
        val box = outbox()
        dictate(box, SttOutcome.Success("first dictation"))
        dictate(box, SttOutcome.Success("second dictation"))
        val queued = box.pending()
        assertEquals(listOf("first dictation", "second dictation"), queued.map { it.body })
        assertEquals(
            listOf(DictationClipFeed.DEVICE, DictationClipFeed.DEVICE),
            queued.map { it.device },
        )
    }

    @Test
    fun onAFallBackFeedsTheTranscriptTheUserSaw() {
        clipSettings().feedDictations = true
        val box = outbox()
        // 401: one upload, then the on-device transcript is what is shown.
        dictate(box, SttOutcome.Http(401, "bad token"), local = "the phone heard this")
        assertEquals(listOf("the phone heard this"), box.pending().map { it.body })
    }

    @Test
    fun aBlankTranscriptIsNotFed() {
        val box = outbox()
        val feed = DictationClipFeed(enabled = { true }, outbox = { box })
        assertFalse(feed.offer("   "))
        assertTrue(box.pending().isEmpty())
    }

    @Test
    fun theTagSurvivesTheQueueFileAndReachesTheFlush() {
        val box = outbox()
        DictationClipFeed(enabled = { true }, outbox = { box }).offer("hello agents")
        box.append("a share-sheet capture")
        val posted = mutableListOf<Pair<String, String>>()
        ClipFlusher(
            outbox = ClipOutboxStore(box.dir),
            device = "pixel-8",
            post = { body, device -> posted += body to device; ClipPost.Stored(1L) },
        ).flush()
        assertEquals(
            listOf("hello agents" to DictationClipFeed.DEVICE, "a share-sheet capture" to "pixel-8"),
            posted,
        )
    }

    @Test
    fun theSettingsToggleSurvivesARestart() {
        val first = Robolectric.buildActivity(SettingsActivity::class.java).setup()
        val toggle = first.get().findViewById<CheckBox>(R.id.clip_feed_dictations)
        assertFalse("Settings offers the feed pre-checked", toggle.isChecked)
        toggle.isChecked = true
        first.get().findViewById<Button>(R.id.clip_save).performClick()
        first.pause().stop().destroy()

        assertTrue("the choice did not reach the store", clipSettings().feedDictations)
        val second = Robolectric.buildActivity(SettingsActivity::class.java).setup()
        assertTrue(
            "a reopened Settings shows the feed off after the user turned it on",
            second.get().findViewById<CheckBox>(R.id.clip_feed_dictations).isChecked,
        )
        second.destroy()
    }

    @Test
    fun thePairingScreenOffersItUnchecked() {
        val c = Robolectric.buildActivity(PairingActivity::class.java).setup()
        assertFalse(c.get().findViewById<CheckBox>(R.id.pairing_feed_dictations).isChecked)
        c.destroy()
    }
}
