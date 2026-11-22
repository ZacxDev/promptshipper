package dev.zacx.promptshipper

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Looper
import android.util.TypedValue
import android.view.ContextThemeWrapper
import android.view.LayoutInflater
import android.view.View
import android.widget.EditText
import android.widget.PopupMenu
import android.widget.TextView
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import java.io.File

/**
 * Taps the card's ⋮ and returns the menu it opened. Shared with
 * [DictateShareTest] and [OverlayNeverStartsARecordingTest], which reach Send
 * and Screenshot this way since those buttons moved into the menu.
 */
internal fun openOverflow(a: DictateActivity): PopupMenu {
    a.findViewById<View>(R.id.overflow_button).performClick()
    shadowOf(Looper.getMainLooper()).idle()
    return requireNotNull(a.overflowMenu) { "tapping ⋮ opened no menu" }
}

/**
 * Opens the ⋮ menu and chooses [itemId] the way a tap does — through
 * `performIdentifierAction`, which REFUSES a disabled item, so a test cannot
 * fire an action the user could not.
 */
internal fun chooseFromOverflow(a: DictateActivity, itemId: Int): Boolean {
    val handled = openOverflow(a).menu.performIdentifierAction(itemId, 0)
    shadowOf(Looper.getMainLooper()).idle()
    return handled
}

/**
 * The card's ⋮ overflow: which actions it holds, that each does what its
 * deleted button did, and that each keeps that button's availability.
 *
 * ## New behaviour vs invariant guard
 *
 * Every test here fails to compile on the pre-change tree (no
 * `overflow_button`, no `menu_*` ids), which is red but says nothing about
 * teeth. Honestly classified:
 *
 *  * NEW BEHAVIOUR: [theMenuHoldsExactlyScreenshotSendAndSaveAudio],
 *    [theThreeStandaloneButtonsAreGone], [theMenuIsThemedToTheDarkCard].
 *  * CARRIED-OVER BEHAVIOUR, now reached through the menu (each was a
 *    button's behaviour before, and is pinned again because the route
 *    changed): [sendFromTheMenuFiresTheSameShareIntent],
 *    [saveAudioIsDisabledOnACardWithNoFiledRecording],
 *    [saveAudioIsEnabledOnAHistoryCardAndHandsOutTheRecording],
 *    [saveAudioStaysDisabledForAMissingHistoryItem].
 *  * INVARIANT GUARD: [screenshotAndSendAreNeverDisabled] — they never were.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class DictateOverflowMenuTest {

    private val ctx: Context get() = RuntimeEnvironment.getApplication()

    @After
    fun tearDown() {
        BubbleSessionBus.clearForTest()
        OverlayVisibility.clearForTest()
        MicrophoneOwner.clearForTest()
        File(ctx.noBackupFilesDir, "history").deleteRecursively()
    }

    private fun plainCard() = Robolectric.buildActivity(DictateActivity::class.java).setup()

    private fun historyCard(id: String) =
        Robolectric.buildActivity(DictateActivity::class.java, DictateLaunch.historyIntent(ctx, id))
            .setup()

    /** A real item in the real store — DictateHistoryCardTest's fixture. */
    private fun store(text: String): String {
        val source = File.createTempFile("dictation-", ".ogg")
        source.writeBytes(ByteArray(64) { it.toByte() })
        try {
            return DictationHistoryStore(ctx).add(source, text, fromRemote = true, audioSeconds = 1.5)
        } finally {
            source.delete()
        }
    }

    /** Waits for the history worker's hand-back, as DictateHistoryCardTest does. */
    private fun awaitStatusChange(a: DictateActivity, from: String): String {
        val read = { a.findViewById<TextView>(R.id.status_text).text.toString() }
        repeat(200) {
            if (read() != from) return read()
            shadowOf(Looper.getMainLooper()).idle()
            Thread.sleep(5)
        }
        return read()
    }

    private fun drainStartedActivities(a: DictateActivity) {
        @Suppress("ControlFlowWithEmptyBody")
        while (shadowOf(a).nextStartedActivity != null) {
        }
    }

    private fun ids(popup: PopupMenu): List<Int> =
        (0 until popup.menu.size()).map { popup.menu.getItem(it).itemId }

    // --- membership ---------------------------------------------------------

    @Test
    fun theMenuHoldsExactlyScreenshotSendAndSaveAudio() {
        val controller = plainCard()
        try {
            val popup = openOverflow(controller.get())
            assertEquals(
                listOf(R.id.menu_screenshot, R.id.menu_share, R.id.menu_save_audio),
                ids(popup),
            )
            assertEquals(
                listOf(R.string.screenshot, R.string.share, R.string.save_audio)
                    .map { ctx.getString(it) },
                (0 until popup.menu.size()).map { popup.menu.getItem(it).title.toString() },
            )
            for (i in 0 until popup.menu.size()) {
                assertTrue(
                    "${popup.menu.getItem(i).title} is hidden — none of the three " +
                        "buttons it replaces was ever hidden",
                    popup.menu.getItem(i).isVisible,
                )
            }
        } finally {
            controller.close()
        }
    }

    @Test
    fun theThreeStandaloneButtonsAreGone() {
        val themed = ContextThemeWrapper(ctx, R.style.Theme_Dicta_Overlay)
        val root = LayoutInflater.from(themed).inflate(R.layout.activity_dictate, null)
        for (name in listOf("screenshot_button", "share_button", "save_audio_button")) {
            val id = ctx.resources.getIdentifier(name, "id", ctx.packageName)
            assertTrue(
                "@id/$name is still in the card — it moved into the ⋮ menu",
                id == 0 || root.findViewById<View>(id) == null,
            )
        }
        val row = root.findViewById<android.view.ViewGroup>(R.id.action_row)
        assertEquals(
            listOf(R.id.copy_button, R.id.overflow_button, R.id.close_button),
            (0 until row.childCount).map { row.getChildAt(it).id },
        )
        val more = root.findViewById<View>(R.id.overflow_button)
        assertEquals(ctx.getString(R.string.more_actions), more.contentDescription?.toString())
        assertEquals(ctx.getString(R.string.more_actions), more.tooltipText?.toString())
    }

    // --- each item does what its button did --------------------------------

    @Test
    fun sendFromTheMenuFiresTheSameShareIntent() {
        // The exact intent DictateShareTest pinned on the old button: a chooser
        // around a text/plain ACTION_SEND carrying the transcript.
        val controller = plainCard()
        try {
            val a = controller.get()
            a.findViewById<EditText>(R.id.dictated_text).setText("hello from the menu")
            drainStartedActivities(a)

            assertTrue(chooseFromOverflow(a, R.id.menu_share))

            val started = shadowOf(a).nextStartedActivity
            assertNotNull("Send in the menu started nothing", started)
            assertEquals(Intent.ACTION_CHOOSER, started!!.action)
            val inner: Intent? = started.getParcelableExtra(Intent.EXTRA_INTENT)
            assertEquals(Intent.ACTION_SEND, inner?.action)
            assertEquals("text/plain", inner?.type)
            assertEquals("hello from the menu", inner?.getStringExtra(Intent.EXTRA_TEXT))
        } finally {
            controller.close()
        }
    }

    // --- availability carries over -----------------------------------------

    @Test
    fun screenshotAndSendAreNeverDisabled() {
        val controller = plainCard()
        try {
            val popup = openOverflow(controller.get())
            assertTrue(popup.menu.findItem(R.id.menu_screenshot).isEnabled)
            assertTrue(popup.menu.findItem(R.id.menu_share).isEnabled)
        } finally {
            controller.close()
        }
    }

    @Test
    fun saveAudioIsDisabledOnACardWithNoFiledRecording() {
        // The old @id/save_audio_button was android:enabled="false" in the XML
        // and stayed so until a recording was filed. A plain card files none.
        val controller = plainCard()
        try {
            val a = controller.get()
            val popup = openOverflow(a)
            assertFalse(
                "Save audio is offered on a card with no recording behind it — " +
                    "the old button was disabled here",
                popup.menu.findItem(R.id.menu_save_audio).isEnabled,
            )
            drainStartedActivities(a)
            assertFalse(
                "a disabled Save audio item could still be dispatched",
                popup.menu.performIdentifierAction(R.id.menu_save_audio, 0),
            )
            assertNull(shadowOf(a).nextStartedActivity)
        } finally {
            controller.close()
        }
    }

    @Test
    fun saveAudioIsEnabledOnAHistoryCardAndHandsOutTheRecording() {
        val id = store("a stored dictation")
        val controller = historyCard(id)
        try {
            val a = controller.get()
            awaitStatusChange(a, "")
            // Wait for renderHistoryItem, the site that enables it.
            repeat(200) {
                if (a.findViewById<View>(R.id.retranscribe_button).isEnabled) return@repeat
                shadowOf(Looper.getMainLooper()).idle()
                Thread.sleep(5)
            }
            assertTrue(
                "precondition: the history item loaded",
                a.findViewById<View>(R.id.retranscribe_button).isEnabled,
            )

            val popup = openOverflow(a)
            assertTrue(
                "Save audio is disabled on a history card, where the old button " +
                    "was enabled",
                popup.menu.findItem(R.id.menu_save_audio).isEnabled,
            )

            drainStartedActivities(a)
            assertTrue(popup.menu.performIdentifierAction(R.id.menu_save_audio, 0))
            var started: Intent? = null
            repeat(200) {
                if (started == null) {
                    shadowOf(Looper.getMainLooper()).idle()
                    started = shadowOf(a).nextStartedActivity
                    if (started == null) Thread.sleep(5)
                }
            }
            assertNotNull("Save audio from the menu started nothing", started)
            assertEquals(Intent.ACTION_CHOOSER, started!!.action)
            val inner: Intent? = started!!.getParcelableExtra(Intent.EXTRA_INTENT)
            assertEquals(Intent.ACTION_SEND, inner?.action)
            assertNotNull(
                "the recording is not attached",
                inner?.getParcelableExtra<Uri>(Intent.EXTRA_STREAM),
            )
            assertFalse(
                "Save audio put the transcript in the share — it is audio only",
                inner!!.hasExtra(Intent.EXTRA_TEXT),
            )
        } finally {
            controller.close()
        }
    }

    @Test
    fun saveAudioStaysDisabledForAMissingHistoryItem() {
        val controller = historyCard("no-such-item")
        try {
            val a = controller.get()
            val status = awaitStatusChange(a, ctx.getString(R.string.history_saved))
            assertEquals("precondition", ctx.getString(R.string.history_missing), status)
            assertFalse(openOverflow(a).menu.findItem(R.id.menu_save_audio).isEnabled)
        } finally {
            controller.close()
        }
    }

    // --- theming -------------------------------------------------------------

    @Test
    fun theMenuIsThemedToTheDarkCard() {
        val controller = plainCard()
        try {
            val popup = openOverflow(controller.get())
            // MenuBuilder.getContext() is hidden API, reachable in Robolectric's
            // android-all; it is the context the menu's views inflate from.
            val menuContext = popup.menu.javaClass.getMethod("getContext")
                .invoke(popup.menu) as Context

            val style = TypedValue()
            assertTrue(
                menuContext.theme.resolveAttribute(android.R.attr.popupMenuStyle, style, true),
            )
            assertEquals(
                "the menu is not wearing Widget.Dicta.PopupMenu, so it shows the " +
                    "parent dialog theme's grey platform sheet",
                R.style.Widget_Dicta_PopupMenu,
                style.resourceId,
            )
            val a = menuContext.obtainStyledAttributes(
                null, intArrayOf(android.R.attr.popupBackground),
                android.R.attr.popupMenuStyle, 0,
            )
            try {
                assertEquals(R.drawable.popup_bg, a.getResourceId(0, 0))
            } finally {
                a.recycle()
            }

            val text = TypedValue()
            assertTrue(
                menuContext.theme.resolveAttribute(android.R.attr.textColorPrimary, text, true),
            )
            val colors = menuContext.getColorStateList(text.resourceId)
            val enabled = colors.getColorForState(intArrayOf(android.R.attr.state_enabled), 0)
            val disabled = colors.getColorForState(intArrayOf(-android.R.attr.state_enabled), 0)
            assertEquals(ctx.getColor(R.color.dicta_on_surface), enabled)
            assertNotEquals(
                "a disabled Save audio would be drawn exactly like an enabled item",
                enabled,
                disabled,
            )
        } finally {
            controller.close()
        }
    }
}
