package dev.zacx.promptshipper

import org.json.JSONObject

/**
 * "Let my agents push to my clipboard": the phone side of the clip server's
 * change feed (`server/clip/README.md`, *Change feed* and *The auto-copy
 * contract*).
 *
 * `GET /v1/clips?after=<seq>&wait=<s>` answers `{"clips":[…],"head":H}` with
 * every clip whose `seq > after`, oldest first, holding the request open up
 * to `wait` seconds when there is none. This file is the policy; [ClipHttp.feed]
 * is the request; `BubbleService` owns the thread and the clipboard.
 *
 * 🔴 **Only `device == "mcp"` is copied.** That is the contract: the MCP server
 * posts as `mcp`, and every other clip (this phone's own captures, the
 * dictation feed, a laptop's snip) is someone's own text, never put on the
 * clipboard unasked.
 *
 * 🔴 **History is never copied.** The first poll of a cursor that has never
 * been set reads only `head` (`wait=0`, `limit=1`) and starts there, so
 * pairing with a server holding a year of clips copies nothing. A cursor
 * AHEAD of `head` (the store was restored from an older backup) is reset to
 * `head` without copying, rather than waiting forever for seqs that may never
 * come — and without replaying the store, which would be the same backlog.
 */
object ClipPush {

    /** The device name whose clips are copied. The server does not enforce it. */
    const val AGENT_DEVICE: String = "mcp"

    /** The long-poll hold, seconds. Under the server's 60 s maximum. */
    const val WAIT_S: Int = 50

    /**
     * The pause after an EMPTY answer. A server honouring `wait` held it for
     * up to [WAIT_S] already; one that answers empty at once (a proxy, a
     * misbehaving build) must not turn the loop into a busy poll.
     */
    const val EMPTY_PAUSE_MS: Long = 1_000L

    /**
     * Rows per poll. Small on purpose: a page is read whole into memory, and
     * [MAX_FEED_BYTES] caps it.
     */
    const val LIMIT: Int = 20

    /** The most a feed answer may be; over it is a failure, never a partial read. */
    const val MAX_FEED_BYTES: Int = 2 * 1024 * 1024

    /** Backoff after a failure: BASE * 2^(n-1), capped at MAX. */
    const val BACKOFF_BASE_MS: Long = 2_000L
    const val BACKOFF_MAX_MS: Long = 300_000L

    /** A `Retry-After` is honoured up to this, so a hostile value cannot park us. */
    const val RETRY_AFTER_MAX_MS: Long = 300_000L

    data class FeedClip(val seq: Long, val device: String, val body: String) {
        override fun toString(): String = "FeedClip(seq=$seq, device=$device, body=<${body.length} chars>)"
    }

    sealed class Poll {
        data class Rows(val clips: List<FeedClip>, val head: Long) : Poll()
        /** 429 or 503 with a `Retry-After`, in milliseconds. */
        data class RetryAfter(val ms: Long) : Poll()
        data class Failed(val reason: String) : Poll()
    }

    fun parse(text: String): Poll {
        val root = try {
            JSONObject(text)
        } catch (_: Exception) {
            return Poll.Failed("the feed answer is not a JSON object")
        }
        if (!root.has("head")) return Poll.Failed("the feed answer has no \"head\" — not a feed-aware server")
        val head = root.optLong("head", -1L)
        if (head < 0L) return Poll.Failed("the feed answer's \"head\" is not usable")
        val array = root.optJSONArray(ClipJson.KEY_CLIPS)
            ?: return Poll.Failed("the feed answer has no \"clips\" array")
        val out = ArrayList<FeedClip>(array.length())
        for (i in 0 until array.length()) {
            val row = array.optJSONObject(i) ?: return Poll.Failed("clip ${i + 1} is not an object")
            val seq = row.optLong("seq", 0L)
            if (seq <= 0L) return Poll.Failed("clip ${i + 1} has no usable \"seq\"")
            out += FeedClip(seq, row.optString(ClipJson.KEY_DEVICE, ""), row.optString(ClipJson.KEY_BODY, ""))
        }
        return Poll.Rows(out, head)
    }

    /**
     * One step of the listener: one poll and its effects. Pure apart from the
     * injected functions, so every rule is testable without a thread, a socket
     * or a clipboard.
     *
     * @param poll `(after, waitSeconds, limit)` -> the server's answer.
     * @param cursor the persisted cursor; null = never set.
     * @param copy puts one agent clip on the clipboard (and shows the notice).
     * @param stillRunning re-checked AFTER the poll returns: a long-poll that
     *   completes after the service stopped must not copy anything.
     * @return how long to wait before the next step, in milliseconds.
     */
    class Loop(
        private val poll: (after: Long, waitSeconds: Int, limit: Int) -> Poll,
        private val loadCursor: () -> Long?,
        private val saveCursor: (Long) -> Unit,
        private val copy: (body: String) -> Unit,
        private val stillRunning: () -> Boolean,
        /**
         * Identifies the clip server a poll was made against (its base URL).
         * Read before and after each poll; a result from a server that was
         * replaced while the poll was held open (a re-pair) is DISCARDED, so
         * the old store's seq never becomes the new store's cursor.
         */
        private val generation: () -> Any? = { null },
    ) {
        /** Consecutive failures; drives the backoff. */
        var failures: Int = 0
            private set

        /**
         * True until the clips already pending when this loop started (with a
         * saved cursor) have been read. Operator decision: of that backlog,
         * only the NEWEST `mcp` clip is copied — a phone that was away for a
         * day gets the agent's latest text, not a burst of every one — and the
         * cursor moves past all of it. Live pushes after that copy as usual.
         */
        private var catchingUp = true
        private var backlogNewest: String? = null

        fun step(): Long {
            val cursor = loadCursor()
            val gen = generation()
            val answer = when {
                cursor == null -> poll(0L, 0, 1)
                catchingUp -> poll(cursor, 0, LIMIT)
                else -> poll(cursor, WAIT_S, LIMIT)
            }
            if (!stillRunning()) return 0L
            if (generation() != gen || loadCursor() != cursor) {
                // The server (or the cursor) changed under the poll: its seqs
                // belong to another store. Discard and start again.
                catchingUp = true
                backlogNewest = null
                return 0L
            }
            return when (answer) {
                is Poll.Rows -> {
                    failures = 0
                    when {
                        // First start: start at head, copy nothing.
                        cursor == null -> {
                            saveCursor(answer.head)
                            catchingUp = false
                            0L
                        }
                        // A cursor ahead of the server: reset, copy nothing.
                        cursor > answer.head -> {
                            saveCursor(answer.head)
                            0L
                        }
                        catchingUp -> catchUp(cursor, answer)
                        else -> live(cursor, answer)
                    }
                }
                is Poll.RetryAfter -> {
                    failures = 0
                    answer.ms.coerceIn(BACKOFF_BASE_MS, RETRY_AFTER_MAX_MS)
                }
                is Poll.Failed -> {
                    failures++
                    backoffMs(failures)
                }
            }
        }

        private fun catchUp(cursor: Long, answer: Poll.Rows): Long {
            var next = cursor
            for (clip in answer.clips.sortedBy { it.seq }) {
                if (clip.seq <= next) continue
                if (clip.device == AGENT_DEVICE && clip.body.isNotEmpty()) backlogNewest = clip.body
                next = clip.seq
            }
            if (next > cursor) saveCursor(next)
            // A short page means the backlog is drained; a full one, read on.
            if (answer.clips.size < LIMIT || next == cursor) {
                catchingUp = false
                backlogNewest?.let { copy(it) }
                backlogNewest = null
            }
            return 0L
        }

        private fun live(cursor: Long, answer: Poll.Rows): Long {
            var next = cursor
            for (clip in answer.clips.sortedBy { it.seq }) {
                if (clip.seq <= next) continue
                if (clip.device == AGENT_DEVICE && clip.body.isNotEmpty()) copy(clip.body)
                next = clip.seq
                // Persist per clip, so a crash mid-batch never re-copies what
                // was already copied.
                saveCursor(next)
            }
            // 🔴 Nothing advanced (an empty answer, or only seqs at or below
            // the cursor): pause, or a server answering at once would make
            // this a busy loop.
            return if (next == cursor) EMPTY_PAUSE_MS else 0L
        }
    }

    fun backoffMs(failures: Int): Long {
        if (failures <= 0) return 0L
        val shift = (failures - 1).coerceAtMost(20)
        return (BACKOFF_BASE_MS shl shift).coerceAtMost(BACKOFF_MAX_MS)
    }
}
