package dev.zacx.promptshipper

/**
 * Runs [ClipPush.Loop] on one daemon thread while `BubbleService` is in the
 * foreground.
 *
 * Battery and network: one long-poll held open by the server (no wake-ups
 * while nothing happens), [ClipPush.backoffMs] after a failure, the server's
 * `Retry-After` on a 429, and `BubbleService` stops it when the bubble stops
 * or the screen has been off for [SCREEN_OFF_GRACE_MS]. It takes no wake
 * lock: with the CPU asleep the poll simply waits.
 *
 * [stop] is final for the current thread: a poll that returns after it copies
 * nothing (the loop's `stillRunning` is re-read after every poll) and no
 * further poll starts.
 */
class ClipPushListener(
    /** Re-read before every poll: the setting and the pairing can change. */
    private val enabled: () -> Boolean,
    private val poll: (after: Long, waitSeconds: Int, limit: Int) -> ClipPush.Poll,
    private val loadCursor: () -> Long?,
    private val saveCursor: (Long) -> Unit,
    private val copy: (String) -> Unit,
    private val sleeper: (Long) -> Unit = { Thread.sleep(it) },
    /** See [ClipPush.Loop]'s `generation`: the clip base URL. */
    private val generation: () -> Any? = { null },
) {
    /** One flag per start, so an old thread exiting cannot clear a new one's. */
    private var alive: java.util.concurrent.atomic.AtomicBoolean? = null
    private var thread: Thread? = null

    val isRunning: Boolean @Synchronized get() = alive?.get() == true

    @Synchronized
    fun start() {
        if (isRunning || !enabled()) return
        val flag = java.util.concurrent.atomic.AtomicBoolean(true)
        alive = flag
        val loop = ClipPush.Loop(
            poll = poll,
            loadCursor = loadCursor,
            saveCursor = saveCursor,
            copy = { if (flag.get()) copy(it) },
            stillRunning = { flag.get() && enabled() },
            generation = generation,
        )
        thread = Thread({ run(loop, flag) }, "clip-push").apply {
            isDaemon = true
            start()
        }
    }

    /**
     * Start or stop to match [enabled] NOW — for a setting toggled, or a
     * pairing completed, while the bubble runs.
     */
    @Synchronized
    fun reconcile() {
        if (enabled()) start() else stop()
    }

    @Synchronized
    fun stop() {
        alive?.set(false)
        alive = null
        thread?.interrupt()
        thread = null
    }

    private fun run(loop: ClipPush.Loop, flag: java.util.concurrent.atomic.AtomicBoolean) {
        while (flag.get() && enabled()) {
            val delay = try {
                loop.step()
            } catch (_: Exception) {
                ClipPush.BACKOFF_MAX_MS
            } catch (_: OutOfMemoryError) {
                // Never take the process down for a clipboard feature.
                ClipPush.BACKOFF_MAX_MS
            }
            if (delay > 0) {
                try {
                    sleeper(delay)
                } catch (_: InterruptedException) {
                    break
                }
            }
        }
        flag.set(false)
    }

    companion object {
        /** How long the screen may be off before the listener stops. */
        const val SCREEN_OFF_GRACE_MS: Long = 5 * 60_000L
    }
}
