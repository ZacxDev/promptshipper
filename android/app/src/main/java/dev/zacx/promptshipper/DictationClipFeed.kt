package dev.zacx.promptshipper

import android.content.Context

/**
 * "Send each dictation to my clip feed" — the OPT-IN path by which a finished
 * transcript reaches the user's agents.
 *
 * 🔴 **Off by default, and the gate is here, not at a call site.** With the
 * setting off, nothing about a dictation leaves the phone except the STT
 * upload itself: [offer] returns before the outbox is even opened. With it on,
 * each finished transcript is appended to the SAME clip outbox the share-sheet
 * capture uses ([ClipOutbox]), tagged [DEVICE], and leaves only through the
 * existing drains — so the outbox's retry, backoff and refusal rules apply to
 * it unchanged. This file and `CaptureActivity.kt` are the outbox's two
 * writers, and `ClipSeamLedgerTest` pins exactly that pair.
 *
 * Called by [DictationSession] once per dictation, at the point the final
 * text is decided: the server's transcript, or the on-device one when the
 * ladder fell back. A dictation with no transcription server configured never
 * reaches either point and is not fed.
 *
 * Never throws: a full disk or an over-long transcript costs the feed item,
 * never the transcript the user is waiting for.
 */
class DictationClipFeed(
    private val enabled: () -> Boolean,
    private val outbox: () -> ClipOutbox,
) {

    /** @return true when [text] was queued. */
    fun offer(text: String): Boolean {
        if (!enabled()) return false
        val body = text.trim()
        if (body.isEmpty()) return false
        return runCatching { outbox().append(body, DEVICE) }.isSuccess
    }

    companion object {
        /** The attribution a fed dictation carries on the clip server. */
        const val DEVICE: String = "phone-dictation"

        fun create(context: Context): DictationClipFeed {
            val app = context.applicationContext
            return DictationClipFeed(
                enabled = { ClipSettings.create(app).feedDictations },
                outbox = { ClipOutboxStore(app) },
            )
        }
    }
}
