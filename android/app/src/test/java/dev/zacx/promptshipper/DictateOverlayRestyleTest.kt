package dev.zacx.promptshipper

import android.content.Context
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.StateListDrawable
import android.view.ContextThemeWrapper
import android.view.LayoutInflater
import android.view.View
import android.widget.ImageButton
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import org.xmlpull.v1.XmlPullParser

/**
 * The 2026-11 overlay restyle: the card matches the bubble, Copy is the
 * primary, Close is red, Re-transcribe is an icon.
 *
 * Every assertion here is NEW BEHAVIOUR and red on the pre-change tree, where
 * Copy wore icon_button_bg with an on-surface tint, Close the same, the card a
 * 1dp divider hairline, and Re-transcribe was a text Button — each assertion
 * names the pre-change value it would have read. Asserted on resolved colours
 * and resource references, never on a drawable's name.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class DictateOverlayRestyleTest {

    private val ctx: Context get() = RuntimeEnvironment.getApplication()

    private fun inflate(): View {
        val themed = ContextThemeWrapper(ctx, R.style.Theme_Dicta_Overlay)
        return LayoutInflater.from(themed).inflate(R.layout.activity_dictate, null)
    }

    /** The colour of [bg]'s shape in [state] (an empty array is "at rest"). */
    private fun fillOf(bg: android.graphics.drawable.Drawable, state: IntArray): Int {
        val list = bg as StateListDrawable
        list.setState(state)
        val shape = list.current as GradientDrawable
        return requireNotNull(shape.color) { "the shape has no solid fill" }.defaultColor
    }

    private val pressed = intArrayOf(android.R.attr.state_pressed, android.R.attr.state_enabled)
    private val rest = intArrayOf(android.R.attr.state_enabled)

    @Test
    fun copyIsThePrimaryActionWithASolidAccentFillAndAPressedState() {
        val copy = inflate().findViewById<ImageButton>(R.id.copy_button)
        assertEquals(
            "Copy's fill at rest is not @color/dicta_accent (it was surface_3 " +
                "under icon_button_bg)",
            ctx.getColor(R.color.dicta_accent),
            fillOf(copy.background, rest),
        )
        assertEquals(
            "Copy has no distinct pressed state",
            ctx.getColor(R.color.dicta_accent_pressed),
            fillOf(copy.background, pressed),
        )
        assertEquals(
            "Copy's glyph is not @color/dicta_on_accent — on an accent fill " +
                "the old on-surface tint is ~1.6:1",
            ctx.getColor(R.color.dicta_on_accent),
            copy.imageTintList?.defaultColor,
        )
    }

    @Test
    fun closeIsRedButLegible() {
        val close = inflate().findViewById<ImageButton>(R.id.close_button)
        assertEquals(
            ctx.getColor(R.color.dicta_error),
            close.imageTintList?.defaultColor,
        )
        assertEquals(
            "Close is not on its red-tinted container",
            ctx.getColor(R.color.dicta_error_container),
            fillOf(close.background, rest),
        )
        // Legibility as a measured number, not a claim: the glyph must clear
        // WCAG 1.4.11's 3:1 for a non-text control against BOTH of its fills.
        for (state in listOf(rest, pressed)) {
            val ratio = contrast(ctx.getColor(R.color.dicta_error), fillOf(close.background, state))
            assertTrue("Close's ✕ is ${"%.2f".format(ratio)}:1 on its fill — under 3:1", ratio >= 3.0)
        }
    }

    @Test
    fun theSecondaryIconControlsAreTintedWithTheAccent() {
        val root = inflate()
        for (id in listOf(R.id.overflow_button, R.id.retranscribe_button)) {
            assertEquals(
                ctx.resources.getResourceEntryName(id) + " is not tinted @color/dicta_accent",
                ctx.getColor(R.color.dicta_accent),
                root.findViewById<ImageButton>(id).imageTintList?.defaultColor,
            )
        }
    }

    @Test
    fun retranscribeIsAnIconButtonCarryingItsWordAsALabel() {
        val view = inflate().findViewById<View>(R.id.retranscribe_button)
        assertTrue(
            "retranscribe_button is a ${view.javaClass.name}, not an ImageButton",
            view is ImageButton && view !is android.widget.Button,
        )
        val label = ctx.getString(R.string.history_retranscribe)
        assertEquals(label, view.contentDescription?.toString())
        assertEquals(label, view.tooltipText?.toString())
    }

    @Test
    fun retranscribeStillRunsOnTheRunningActivity() {
        // The cast in DictateActivity.onCreate executes and the listener is wired.
        val controller = Robolectric.buildActivity(DictateActivity::class.java).setup()
        try {
            val v = controller.get().findViewById<View>(R.id.retranscribe_button)
            assertTrue(v.hasOnClickListeners())
            assertEquals("hidden on a live card, as before", View.GONE, v.visibility)
        } finally {
            controller.close()
        }
    }

    @Test
    fun theCardWearsTheBubblesTwoDpAccentRing() {
        // GradientDrawable has no stroke getter, so the compiled XML is read
        // instead: the <stroke> element's resolved width and colour reference.
        val parser = ctx.resources.getXml(R.drawable.card_bg)
        var width: String? = null
        var color = 0
        var fill = 0
        while (parser.next() != XmlPullParser.END_DOCUMENT) {
            if (parser.eventType != XmlPullParser.START_TAG) continue
            when (parser.name) {
                "stroke" -> {
                    width = parser.getAttributeValue(ANDROID, "width")
                    color = parser.getAttributeResourceValue(ANDROID, "color", 0)
                }
                "solid" -> fill = parser.getAttributeResourceValue(ANDROID, "color", 0)
            }
        }
        assertEquals("the card's ring is not @color/dicta_accent", R.color.dicta_accent, color)
        assertTrue("the card's ring is $width, not 2dp", width?.startsWith("2.0") == true)
        assertEquals("the card lost its dark fill", R.color.dicta_surface_2, fill)
    }

    private fun contrast(a: Int, b: Int): Double {
        fun lum(c: Int): Double {
            fun ch(v: Int): Double {
                val s = v / 255.0
                return if (s <= 0.04045) s / 12.92 else Math.pow((s + 0.055) / 1.055, 2.4)
            }
            return 0.2126 * ch((c shr 16) and 0xFF) + 0.7152 * ch((c shr 8) and 0xFF) +
                0.0722 * ch(c and 0xFF)
        }
        val (hi, lo) = listOf(lum(a), lum(b)).sortedDescending()
        return (hi + 0.05) / (lo + 0.05)
    }

    private companion object {
        const val ANDROID = "http://schemas.android.com/apk/res/android"
    }
}
