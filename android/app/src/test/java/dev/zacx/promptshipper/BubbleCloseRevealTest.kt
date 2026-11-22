package dev.zacx.promptshipper

import android.Manifest
import android.content.Context
import android.os.Looper
import android.os.SystemClock
import android.view.MotionEvent
import android.view.View
import android.view.WindowManager
import android.widget.ImageButton
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import org.robolectric.shadows.ShadowSettings
import java.time.Duration

/**
 * The close button inside the long-press reveal, driven through the real
 * [BubbleService] with real `MotionEvent`s — [BubbleClipRevealTest]'s harness.
 *
 * ## What is new behaviour and what is an invariant guard
 *
 * Every test here is red on the pre-change tree, because there is no `close`
 * field to read (the reflective lookup throws) and no control to tap. That
 * says nothing about which assertions have teeth, so, honestly:
 *
 *  * NEW BEHAVIOUR: [theCloseButtonIsGoneBeforeALongPressAndVisibleAfter],
 *    [tappingCloseStopsTheServiceExactlyAsHideBubbleDoes],
 *    [afterCloseTheBubbleWindowIsRemovedWhenTheServiceGoes].
 *  * Mutation-killable relationship guards:
 *    [theFourRevealButtonsAreRaisedAndLoweredTOGETHER] (a missed line in
 *    `showActions`), [theWindowHeightAccountsForTheCloseButton] (a missed term
 *    in `windowHeightPx`, with its own reachability control).
 *  * INVARIANT GUARDS: [tappingCloseOpensNothingAndRecordsNothing],
 *    [theCloseButtonIsNotTheCancelButton], [theCloseButtonIsInsideTheBubblesOwnWindow].
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34], qualifiers = "w411dp-h914dp-port-xxhdpi")
class BubbleCloseRevealTest {

    private var controller: org.robolectric.android.controller.ServiceController<BubbleService>? =
        null

    private class FakeSession : BubbleService.SessionHandle {
        var starts = 0
        override fun start(): Boolean { starts++; return true }
        override fun submit() = Unit
        override fun discard() = Unit
        override fun destroy() = Unit
    }

    private val fake = FakeSession()

    @Before
    fun setUp() {
        ClipOutboxHealth.clearForTest()
    }

    @After
    fun tearDown() {
        runCatching { controller?.destroy() }
        OverlayVisibility.clearForTest()
        BubbleSessionBus.clearForTest()
        MicrophoneOwner.clearForTest()
        ClipOutboxHealth.clearForTest()
    }

    private val app get() = RuntimeEnvironment.getApplication()

    /** The raw prefs, spelled out for BubbleHideFromNotificationTest's reason. */
    private fun flag(): Boolean = app.getSharedPreferences("dicta", Context.MODE_PRIVATE)
        .getBoolean("bubble_enabled", false)

    private fun start(): BubbleService {
        ShadowSettings.setCanDrawOverlays(true)
        shadowOf(app).grantPermissions(Manifest.permission.RECORD_AUDIO)
        app.getSharedPreferences("dicta", Context.MODE_PRIVATE)
            .edit().putBoolean("bubble_enabled", true).commit()
        val c = Robolectric.buildService(BubbleService::class.java)
        controller = c
        c.get().sessionFactory = { fake }
        c.get().clipFlush = {}
        c.get().clipFlushExecutor = java.util.concurrent.Executor { }
        return c.create().get()
    }

    private fun <T> field(service: BubbleService, name: String): T {
        val f = BubbleService::class.java.getDeclaredField(name)
        f.isAccessible = true
        @Suppress("UNCHECKED_CAST")
        return f.get(service) as T
    }

    private fun bubble(service: BubbleService): View =
        requireNotNull(field<View?>(service, "bubble")) { "no bubble view" }

    private fun close(service: BubbleService): ImageButton =
        requireNotNull(field<ImageButton?>(service, "close")) { "no close button" }

    private fun button(service: BubbleService, name: String): ImageButton =
        requireNotNull(field<ImageButton?>(service, name)) { "no $name button" }

    private fun params(service: BubbleService): WindowManager.LayoutParams =
        field(service, "params")

    private fun idle(ms: Long) {
        shadowOf(Looper.getMainLooper()).idleFor(Duration.ofMillis(ms))
    }

    private fun longPress(service: BubbleService) {
        val v = bubble(service)
        val t = SystemClock.uptimeMillis()
        v.dispatchTouchEvent(MotionEvent.obtain(t, t, MotionEvent.ACTION_DOWN, 10f, 10f, 0))
        idle(BubbleGesture.LONG_PRESS_MS + 50)
        val u = SystemClock.uptimeMillis()
        v.dispatchTouchEvent(MotionEvent.obtain(u, u, MotionEvent.ACTION_UP, 10f, 10f, 0))
        idle(0)
    }

    private fun dragTo(service: BubbleService, y: Float) {
        val v = bubble(service)
        val t = SystemClock.uptimeMillis()
        v.dispatchTouchEvent(MotionEvent.obtain(t, t, MotionEvent.ACTION_DOWN, 10f, 10f, 0))
        v.dispatchTouchEvent(MotionEvent.obtain(t, t, MotionEvent.ACTION_MOVE, 10f, y, 0))
        v.dispatchTouchEvent(MotionEvent.obtain(t, t, MotionEvent.ACTION_UP, 10f, 10f, 0))
        idle(0)
    }

    private val screenHeight: Int
        get() = app.resources.displayMetrics.heightPixels

    // --- visibility is the reveal's -------------------------------------------

    @Test
    fun theCloseButtonIsGoneBeforeALongPressAndVisibleAfter() {
        val service = start()
        assertEquals(
            "the close button is on screen with no reveal up — an always-on " +
                "control that turns the bubble off",
            View.GONE,
            close(service).visibility,
        )
        longPress(service)
        assertEquals(View.VISIBLE, close(service).visibility)
    }

    @Test
    fun theFourRevealButtonsAreRaisedAndLoweredTOGETHER() {
        val service = start()
        val all = {
            listOf("history", "snip", "clip", "close").map { button(service, it).visibility }
        }
        val gone = listOf(View.GONE, View.GONE, View.GONE, View.GONE)
        val shown = listOf(View.VISIBLE, View.VISIBLE, View.VISIBLE, View.VISIBLE)

        assertEquals(gone, all())
        longPress(service)
        assertEquals(shown, all())

        // Dismissed by an outside touch ...
        val t = SystemClock.uptimeMillis()
        requireNotNull(field<View?>(service, "root"))
            .dispatchTouchEvent(MotionEvent.obtain(t, t, MotionEvent.ACTION_OUTSIDE, 0f, 0f, 0))
        idle(0)
        assertEquals(
            "an outside touch put the other reveal buttons away and left " +
                "close behind, so its visibility is owned somewhere else",
            gone,
            all(),
        )

        // ... and by a tap on the bubble (which also starts a recording).
        longPress(service)
        assertEquals(shown, all())
        bubble(service).performClick()
        idle(0)
        assertEquals(gone, all())
        assertEquals(
            "the close button is on screen over a live recording",
            View.GONE,
            close(service).visibility,
        )
    }

    // --- the stop ----------------------------------------------------------------

    @Test
    fun tappingCloseStopsTheServiceExactlyAsHideBubbleDoes() {
        val service = start()
        assertTrue("precondition: the fixture enabled the bubble", flag())
        assertFalse(
            "precondition: the service is not already stopping",
            shadowOf(service).isStoppedBySelf,
        )

        longPress(service)
        close(service).performClick()
        idle(0)

        assertTrue(
            "tapping close did not stop the service",
            shadowOf(service).isStoppedBySelf,
        )
        assertFalse(
            "close stopped the service but left bubble_enabled=true — so the " +
                "setup screen's switch reads ON with nothing running, which " +
                "is issue #14 re-created through a second stop path",
            flag(),
        )
        assertEquals(
            "the reveal was left armed with FLAG_WATCH_OUTSIDE_TOUCH",
            0,
            params(service).flags and WindowManager.LayoutParams.FLAG_WATCH_OUTSIDE_TOUCH,
        )
    }

    @Test
    fun afterCloseTheBubbleWindowIsRemovedWhenTheServiceGoes() {
        // stopSelf() does not run onDestroy under Robolectric, so the platform's
        // half of the stop is driven by hand — and the window must then be gone,
        // the same observable as ACTION_STOP.
        val service = start()
        idle(0)
        val window = requireNotNull(field<View?>(service, "root"))
        assertTrue(
            "precondition (positive control): the bubble window is attached " +
                "before close, so 'not attached' below is a measurement",
            window.isAttachedToWindow,
        )

        longPress(service)
        close(service).performClick()
        idle(0)
        assertTrue("precondition: close stopped the service", shadowOf(service).isStoppedBySelf)

        controller?.destroy()
        controller = null
        idle(0)

        assertFalse("the bubble window outlived the service", window.isAttachedToWindow)
        assertNull("the service still holds its window", field<View?>(service, "root"))
    }

    @Test
    fun tappingCloseOpensNothingAndRecordsNothing() {
        val service = start()
        longPress(service)
        close(service).performClick()
        idle(0)

        assertNull(
            "close started an activity — it must only turn the bubble off",
            shadowOf(service).nextStartedActivity,
        )
        assertEquals("close opened the microphone", 0, fake.starts)
        assertNull("close left a session behind", field<Any?>(service, "session"))
    }

    // --- identity ----------------------------------------------------------------

    @Test
    fun theCloseButtonIsNotTheCancelButton() {
        // Both draw ic_close. One discards a recording, the other turns the
        // bubble off; they must not announce or look like the same control.
        val service = start()
        val close = close(service)
        val cancel = button(service, "cancel")
        assertNotEquals(close, cancel)
        assertEquals(app.getString(R.string.bubble_close), close.contentDescription?.toString())
        assertEquals(close.contentDescription?.toString(), close.tooltipText?.toString())
        assertNotEquals(
            "the close and cancel ✕ announce the same thing",
            cancel.contentDescription?.toString(),
            close.contentDescription?.toString(),
        )
        assertEquals(
            "the close button is not tinted @color/dicta_error",
            app.getColor(R.color.dicta_error),
            close.imageTintList?.defaultColor,
        )
        assertNotEquals(
            cancel.imageTintList?.defaultColor,
            close.imageTintList?.defaultColor,
        )
        val labels = listOf("history", "snip", "clip", "close")
            .map { button(service, it).contentDescription?.toString() }
        assertEquals("two reveal buttons announce the same thing: $labels", 4, labels.toSet().size)
    }

    @Test
    fun theCloseButtonIsInsideTheBubblesOwnWindow() {
        val service = start()
        assertSame(
            "the close button is not a child of the bubble's own window, so it " +
                "is a second overlay and needs its own OverlayVisibility sink",
            field<View?>(service, "root"),
            close(service).parent,
        )
    }

    // --- the clamp -----------------------------------------------------------------

    @Test
    fun theWindowHeightAccountsForTheCloseButton() {
        // 🔴 Close is now the BOTTOM-MOST reveal control, so dropping its term
        // from windowHeightPx() leaves exactly it off the bottom of the screen
        // for a bubble parked low. Same shape as BubbleClipRevealTest's pair:
        // the exact clamped position, plus the control that the fourth term
        // is observably load-bearing on this display.
        val service = start()
        longPress(service)
        val halo: Int = field(service, "haloSizePx")
        val history: Int = field(service, "historyBlockPx")
        val snip: Int = field(service, "snipBlockPx")
        val clip: Int = field(service, "clipBlockPx")
        val close: Int = field(service, "closeBlockPx")
        assertTrue("precondition: the close block has a height", close > 0)

        dragTo(service, 9000f)
        val y = params(service).y

        val threeBlockBottom = screenHeight - (halo + history + snip + clip)
        assertTrue(
            "the clamp put the window at y=$y, where a THREE-block reveal " +
                "would leave it ($threeBlockBottom) — so this display cannot " +
                "tell whether the close button is in the clamp",
            y < threeBlockBottom,
        )
        assertEquals(
            screenHeight - (halo + history + snip + clip + close),
            y,
        )
    }
}
