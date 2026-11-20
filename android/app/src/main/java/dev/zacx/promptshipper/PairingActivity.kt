package dev.zacx.promptshipper

import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.os.Bundle
import android.text.Editable
import android.text.TextWatcher
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast

/**
 * First-run pairing: read the v1 payload `./setup` prints (`docs/pairing.md`),
 * show what it is about to save, and save it on one tap.
 *
 * ## How the QR code is read — and why there is no scanner library
 *
 * The APK ships **no runtime dependencies** (`app/build.gradle.kts`), and a QR
 * decoder is a big one: ZXing's core is ~500 KB of code, and Google's code
 * scanner is a Play-services module, which a de-Googled phone does not have.
 * Neither is needed, because the payload is plain JSON text:
 *
 *  1. **Scan** hands off to any installed scanner app that answers the
 *     long-standing ZXing intent `com.google.zxing.client.android.SCAN`
 *     (Binary Eye, ZXing Barcode Scanner, and others) and reads its
 *     `SCAN_RESULT`. No permission: the scanner app owns the camera.
 *  2. Without one, the phone's own camera app reads QR codes on most current
 *     phones and offers to copy the text — then **Paste** reads the clipboard.
 *  3. Or the text can be pasted from anywhere (`./setup --pairing` prints it).
 *
 * All three end in the same [PairingPayload.parse], so they cannot disagree.
 *
 * 🔴 Nothing is written until [R.id.pair_confirm]. A payload is two bearer
 * tokens plus two addresses; the confirmation shows the addresses so a code
 * from somewhere unexpected is not applied blind. `exported="false"`: no other
 * app can start this screen or hand it a payload.
 */
class PairingActivity : Activity() {

    private lateinit var input: EditText
    private lateinit var status: TextView
    private lateinit var confirm: Button
    private var parsed: PairingPayload.Pairing? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_pairing)
        input = findViewById(R.id.pairing_input)
        status = findViewById(R.id.pairing_status)
        confirm = findViewById(R.id.pair_confirm)

        findViewById<Button>(R.id.pairing_scan).setOnClickListener { scan() }
        findViewById<Button>(R.id.pairing_paste).setOnClickListener { paste() }
        confirm.setOnClickListener { apply() }
        input.addTextChangedListener(object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) {}
            override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) {}
            override fun afterTextChanged(s: Editable?) = evaluate(s?.toString().orEmpty())
        })
        evaluate("")
    }

    private fun scan() {
        val intent = Intent(SCAN_ACTION)
            .putExtra("SCAN_MODE", "QR_CODE_MODE")
            .putExtra("SAVE_HISTORY", false)
            .putExtra("PROMPT_MESSAGE", getString(R.string.pairing_scan_prompt))
        try {
            @Suppress("DEPRECATION")
            startActivityForResult(intent, REQ_SCAN)
        } catch (_: ActivityNotFoundException) {
            status.text = getString(R.string.pairing_no_scanner)
        }
    }

    @Deprecated("Platform API; the app has no androidx.")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQ_SCAN) return
        val text = data?.getStringExtra(SCAN_RESULT)
        if (resultCode != RESULT_OK || text == null) {
            status.text = getString(R.string.pairing_scan_cancelled)
            return
        }
        input.setText(text)
    }

    private fun paste() {
        val cm = getSystemService(ClipboardManager::class.java)
        val text = cm?.primaryClip
            ?.takeIf { it.itemCount > 0 }
            ?.getItemAt(0)
            ?.coerceToText(this)
            ?.toString()
        if (text.isNullOrBlank()) {
            status.text = getString(R.string.pairing_clipboard_empty)
            return
        }
        input.setText(text)
    }

    /** Re-validates on every edit; only a valid payload enables [confirm]. */
    private fun evaluate(text: String) {
        if (text.isBlank()) {
            parsed = null
            confirm.isEnabled = false
            status.text = getString(R.string.pairing_waiting)
            return
        }
        when (val r = PairingPayload.parse(text)) {
            is PairingPayload.Result.Ok -> {
                parsed = r.pairing
                confirm.isEnabled = true
                val replacing = PairingWriter.isPaired(SttSettings.create(this))
                status.text = getString(
                    if (replacing) R.string.pairing_ready_replace else R.string.pairing_ready,
                    r.pairing.displayName,
                    r.pairing.stt.url,
                    r.pairing.clip.url,
                )
            }
            is PairingPayload.Result.Rejected -> {
                parsed = null
                confirm.isEnabled = false
                status.text = getString(R.string.pairing_rejected, r.reason)
            }
        }
    }

    private fun apply() {
        val pairing = parsed ?: return
        try {
            PairingWriter.apply(pairing, SttSettings.create(this), ClipSettings.create(this))
        } catch (e: RuntimeException) {
            status.text = getString(R.string.pairing_failed, e.javaClass.simpleName)
            return
        }
        val pasted = input.text.toString()
        input.setText("")
        clearClipboardIfItHolds(pasted)
        Toast.makeText(this, getString(R.string.pairing_done, pairing.displayName), Toast.LENGTH_SHORT)
            .show()
        setResult(RESULT_OK)
        finish()
    }

    /**
     * The payload is two bearer tokens; if it came through the clipboard, do
     * not leave it there for the next app that reads it.
     */
    private fun clearClipboardIfItHolds(text: String) {
        val cm = getSystemService(ClipboardManager::class.java) ?: return
        val current = cm.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.text?.toString()
        if (current != null && current.trim() == text.trim()) {
            cm.setPrimaryClip(ClipData.newPlainText("", ""))
        }
    }

    companion object {
        /** The ZXing scan intent, answered by most Android QR scanner apps. */
        const val SCAN_ACTION: String = "com.google.zxing.client.android.SCAN"
        const val SCAN_RESULT: String = "SCAN_RESULT"
        private const val REQ_SCAN = 4101
    }
}
