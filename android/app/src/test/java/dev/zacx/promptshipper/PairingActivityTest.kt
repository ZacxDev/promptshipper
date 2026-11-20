package dev.zacx.promptshipper

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
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

/**
 * The first-run pairing screen, driven through its own controls.
 *
 * `SttSettings.create`/`ClipSettings.create` use the AndroidKeyStore, which
 * Robolectric does not have, so a CONFIRMED pairing surfaces as the screen's
 * own "Pairing failed … nothing was changed" path here; the write itself is
 * covered by [PairingPayloadTest] with an injected cipher. What this file pins
 * is the screen: nothing is written before Pair, Pair is only enabled for a
 * valid payload, and Scan hands off to a ZXing-intent scanner and reads its
 * result.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class PairingActivityTest {

    private val ctx get() = RuntimeEnvironment.getApplication()

    private val payload =
        """{"v":1,"type":"promptshipper-pairing","name":"box",""" +
            """"stt":{"url":"http://100.101.102.103:8118","token":"${"a".repeat(64)}"},""" +
            """"clip":{"url":"http://100.101.102.103:8139","token":"${"b".repeat(64)}"}}"""

    private fun launch() = Robolectric.buildActivity(PairingActivity::class.java).setup()

    @Test
    fun aValidPayloadEnablesPairAndShowsBothAddressesButWritesNothing() {
        val c = launch()
        val a = c.get()
        val pair = a.findViewById<Button>(R.id.pair_confirm)
        assertFalse("Pair is enabled with nothing entered", pair.isEnabled)

        a.findViewById<EditText>(R.id.pairing_input).setText(payload)
        assertTrue("a valid payload did not enable Pair", pair.isEnabled)
        val status = a.findViewById<TextView>(R.id.pairing_status).text.toString()
        assertTrue(status, status.contains("http://100.101.102.103:8118"))
        assertTrue(status, status.contains("http://100.101.102.103:8139"))
        assertFalse("the status line shows a token", status.contains("a".repeat(24)))
        // Nothing written before the user confirms.
        assertNull(SttSettings.create(ctx).baseUrl)
        assertNull(ClipSettings.create(ctx).baseUrl)
        c.destroy()
    }

    @Test
    fun anInvalidPayloadSaysWhyAndKeepsPairDisabled() {
        val c = launch()
        val a = c.get()
        a.findViewById<EditText>(R.id.pairing_input)
            .setText(payload.replace("\"v\":1", "\"v\":2"))
        assertFalse(a.findViewById<Button>(R.id.pair_confirm).isEnabled)
        val status = a.findViewById<TextView>(R.id.pairing_status).text.toString()
        assertTrue(status, status.contains("update promptshipper"))
        c.destroy()
    }

    @Test
    fun scanHandsOffToAZxingIntentScannerAndReadsItsResult() {
        val c = launch()
        val a = c.get()
        a.findViewById<Button>(R.id.pairing_scan).performClick()
        val started = shadowOf(a).nextStartedActivityForResult
        assertNotNull("Scan started nothing", started)
        assertEquals(PairingActivity.SCAN_ACTION, started.intent.action)
        assertEquals("QR_CODE_MODE", started.intent.getStringExtra("SCAN_MODE"))

        shadowOf(a).receiveResult(
            started.intent,
            android.app.Activity.RESULT_OK,
            Intent().putExtra(PairingActivity.SCAN_RESULT, payload + "\n"),
        )
        assertEquals(payload + "\n", a.findViewById<EditText>(R.id.pairing_input).text.toString())
        assertTrue(a.findViewById<Button>(R.id.pair_confirm).isEnabled)
        c.destroy()
    }

    @Test
    fun pasteReadsTheClipboard() {
        ctx.getSystemService(ClipboardManager::class.java)
            .setPrimaryClip(ClipData.newPlainText("", payload))
        val c = launch()
        val a = c.get()
        a.findViewById<Button>(R.id.pairing_paste).performClick()
        assertEquals(payload, a.findViewById<EditText>(R.id.pairing_input).text.toString())
        assertTrue(a.findViewById<Button>(R.id.pair_confirm).isEnabled)
        c.destroy()
    }

    @Test
    fun theInputDoesNotPersistTokensInSavedState() {
        val c = launch()
        val input = c.get().findViewById<EditText>(R.id.pairing_input)
        assertFalse("the pairing field would save its tokens into instance state", input.isSaveEnabled)
        c.destroy()
    }

    @Test
    fun theScreenIsNotExported() {
        val info = ctx.packageManager.getActivityInfo(
            android.content.ComponentName(ctx, PairingActivity::class.java), 0,
        )
        assertFalse("another app could hand this screen a payload", info.exported)
    }
}
