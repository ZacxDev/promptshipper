package dev.zacx.promptshipper

import android.content.Context
import android.widget.CheckBox
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import org.robolectric.Shadows.shadowOf
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * The agent push ([ClipPush], [ClipPushListener]) against the clip server's
 * change-feed contract (`server/clip/README.md`, *Change feed*): only `mcp`
 * clips are copied; the cursor persists, starts at `head` on first start, and
 * resets to `head` when it is ahead; failures back off; stop means stop.
 *
 * Fixture seqs and heads are pairwise distinct and distinct from every
 * constant the code names (0, 1, [ClipPush.LIMIT], [ClipPush.WAIT_S]).
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class ClipPushTest {

    private class Server(vararg answers: ClipPush.Poll) {
        val queue = ArrayDeque(answers.toList())
        val calls = mutableListOf<Triple<Long, Int, Int>>()
        fun poll(after: Long, wait: Int, limit: Int): ClipPush.Poll {
            calls += Triple(after, wait, limit)
            return queue.removeFirstOrNull() ?: ClipPush.Poll.Rows(emptyList(), after)
        }
    }

    private class Cursor(var value: Long?) {
        val saves = mutableListOf<Long>()
    }

    private fun loop(server: Server, cursor: Cursor, copied: MutableList<String>, running: () -> Boolean = { true }) =
        ClipPush.Loop(
            poll = server::poll,
            loadCursor = { cursor.value },
            saveCursor = { cursor.value = it; cursor.saves += it },
            copy = { copied += it },
            stillRunning = running,
        )

    private fun clip(seq: Long, device: String, body: String) = ClipPush.FeedClip(seq, device, body)

    /** An empty catch-up answer at [head]: the loop then reads live pushes. */
    private fun caughtUp(head: Long) = ClipPush.Poll.Rows(emptyList(), head)

    @Test
    fun onlyMcpClipsAreCopied() {
        val server = Server(
            caughtUp(730),
            ClipPush.Poll.Rows(
                listOf(
                    clip(731, "mcp", "from the agent"),
                    clip(732, "pixel-8", "my own capture"),
                    clip(733, DictationClipFeed.DEVICE, "my dictation"),
                    clip(734, "MCP", "not exactly mcp"),
                    clip(735, "mcp", "second agent text"),
                ),
                head = 735,
            )
        )
        val copied = mutableListOf<String>()
        val cursor = Cursor(730)
        val l = loop(server, cursor, copied)
        l.step()
        l.step()
        assertEquals(listOf("from the agent", "second agent text"), copied)
        assertEquals("the cursor is the max seq seen, copied or not", 735L, cursor.value)
    }

    @Test
    fun theCursorIsPersistedAndTheNextPollStartsFromIt() {
        val server = Server(
            caughtUp(407),
            ClipPush.Poll.Rows(listOf(clip(412, "pixel-8", "a"), clip(415, "mcp", "b")), head = 415),
        )
        val cursor = Cursor(407)
        val copied = mutableListOf<String>()
        val first = loop(server, cursor, copied)
        first.step()
        first.step()
        // A NEW loop (a restart) over the same persisted cursor.
        loop(server, cursor, copied).step()
        assertEquals("catch-up reads without waiting", Triple(407L, 0, ClipPush.LIMIT), server.calls[0])
        assertEquals(Triple(407L, ClipPush.WAIT_S, ClipPush.LIMIT), server.calls[1])
        assertEquals("the restart did not resume at the persisted cursor", 415L, server.calls[2].first)
        assertEquals(listOf("b"), copied)
    }

    @Test
    fun theSettingsCursorSurvivesARestart() {
        val ctx = RuntimeEnvironment.getApplication()
        val p = SttSettings.prefs(ctx)
        fun settings() = ClipSettings(p, TokenStore(p, object : SecretCipher {
            override fun encrypt(plaintext: String) = plaintext
            override fun decrypt(blob: String): String? = blob
        }, ClipSettings.KEY_TOKEN_CIPHERTEXT))
        assertNull("a never-polled cursor is unset", settings().pushCursor)
        settings().pushCursor = 9_183L
        assertEquals(9_183L, settings().pushCursor)
    }

    @Test
    fun aCursorAheadOfHeadIsResetToHeadAndCopiesNothing() {
        val server = Server(ClipPush.Poll.Rows(listOf(clip(57, "mcp", "old store text")), head = 58))
        val cursor = Cursor(4_096)
        val copied = mutableListOf<String>()
        loop(server, cursor, copied).step()
        assertEquals(58L, cursor.value)
        assertTrue("a reset replayed the store: $copied", copied.isEmpty())
    }

    @Test
    fun firstStartBeginsAtHeadAndDoesNotCopyTheBacklog() {
        val server = Server(
            ClipPush.Poll.Rows(listOf(clip(3, "mcp", "a year-old agent push")), head = 2_604),
        )
        val cursor = Cursor(null)
        val copied = mutableListOf<String>()
        loop(server, cursor, copied).step()
        assertEquals("the first poll must not wait and needs one row", Triple(0L, 0, 1), server.calls.single())
        assertTrue("the backlog was copied on first start: $copied", copied.isEmpty())
        assertEquals(2_604L, cursor.value)
    }

    @Test
    fun failuresBackOffExponentiallyToACapAndResetOnSuccess() {
        val server = Server(
            caughtUp(77),
            ClipPush.Poll.Failed("x"), ClipPush.Poll.Failed("x"), ClipPush.Poll.Failed("x"),
            ClipPush.Poll.Rows(emptyList(), head = 77), ClipPush.Poll.Failed("x"),
        )
        val l = loop(server, Cursor(77), mutableListOf())
        assertEquals(0L, l.step())
        assertEquals(listOf(2_000L, 4_000L, 8_000L), List(3) { l.step() })
        assertEquals("an empty answer pauses briefly, never busy-polls", ClipPush.EMPTY_PAUSE_MS, l.step())
        assertEquals("a success resets the backoff", 2_000L, l.step())
        assertEquals(ClipPush.BACKOFF_MAX_MS, ClipPush.backoffMs(40))
        assertTrue(ClipPush.backoffMs(8) <= ClipPush.BACKOFF_MAX_MS)
    }

    @Test
    fun anAnswerThatDoesNotAdvanceTheCursorPausesInsteadOfSpinning() {
        // Round 1 🟡2: rows all at or below the cursor used to return 0.
        val l = loop(
            Server(caughtUp(5), ClipPush.Poll.Rows(listOf(clip(3, "mcp", "stale")), head = 10)),
            Cursor(5), mutableListOf(),
        )
        l.step()
        assertEquals(ClipPush.EMPTY_PAUSE_MS, l.step())
    }

    @Test
    fun aRestartCopiesOnlyTheNewestPendingAgentClipThenLivePushesAsUsual() {
        // Round 1 🟡3, operator-delegated: of the backlog pending at (re)start,
        // copy only the newest mcp clip, and move the cursor past all of it.
        val server = Server(
            ClipPush.Poll.Rows(
                listOf(
                    clip(601, "mcp", "agent 1"), clip(602, "mcp", "agent 2"),
                    clip(603, "pixel-8", "a capture"), clip(604, "mcp", "agent 3"),
                    clip(605, "mcp", "agent 4"), clip(606, "mcp", "agent 5"),
                    clip(607, "laptop", "newest but not an agent"),
                ),
                head = 607,
            ),
            ClipPush.Poll.Rows(listOf(clip(612, "mcp", "live one"), clip(613, "mcp", "live two")), head = 613),
        )
        val cursor = Cursor(600)
        val copied = mutableListOf<String>()
        val l = loop(server, cursor, copied)
        l.step()
        assertEquals(listOf("agent 5"), copied)
        assertEquals(607L, cursor.value)
        l.step()
        assertEquals("live pushes after catch-up copy as before", listOf("agent 5", "live one", "live two"), copied)
    }

    @Test
    fun aBacklogLongerThanOnePageIsReadThroughBeforeTheNewestIsCopied() {
        val page1 = (1..ClipPush.LIMIT).map { clip(1_000L + it, "mcp", "old $it") }
        val server = Server(
            ClipPush.Poll.Rows(page1, head = 1_037),
            ClipPush.Poll.Rows(listOf(clip(1_037, "mcp", "newest of the backlog")), head = 1_037),
        )
        val cursor = Cursor(1_000)
        val copied = mutableListOf<String>()
        val l = loop(server, cursor, copied)
        l.step()
        assertTrue("copied before the backlog was drained: $copied", copied.isEmpty())
        l.step()
        assertEquals(listOf("newest of the backlog"), copied)
        assertEquals(1_037L, cursor.value)
    }

    @Test
    fun aPollAnsweredByAServerThatWasReplacedMeanwhileIsDiscarded() {
        // Round 1 🟡4: a re-pair while a poll is held open.
        var base = "http://100.101.102.103:8139"
        val cursor = Cursor(880)
        val copied = mutableListOf<String>()
        val server = object {
            var calls = 0
            fun poll(a: Long, w: Int, l: Int): ClipPush.Poll {
                calls++
                if (calls == 2) base = "http://192.168.1.20:8139" // re-paired mid-poll
                return if (calls == 1) caughtUp(880)
                else ClipPush.Poll.Rows(listOf(clip(9_999, "mcp", "old server's text")), head = 9_999)
            }
        }
        val l = ClipPush.Loop(
            server::poll, { cursor.value }, { cursor.value = it }, { copied += it }, { true },
            generation = { base },
        )
        l.step()
        assertEquals(0L, l.step())
        assertEquals("the old server's seq became the cursor", 880L, cursor.value)
        assertTrue(copied.isEmpty())
    }

    @Test
    fun tooManyWaitersHonoursRetryAfterWithinBounds() {
        val l = loop(Server(caughtUp(5), ClipPush.Poll.RetryAfter(7_000), ClipPush.Poll.RetryAfter(86_400_000)), Cursor(5), mutableListOf())
        l.step()
        assertEquals(7_000L, l.step())
        assertEquals(ClipPush.RETRY_AFTER_MAX_MS, l.step())
    }

    @Test
    fun aPollThatReturnsAfterStopCopiesNothing() {
        var running = true
        val server = object {
            fun poll(a: Long, w: Int, l: Int): ClipPush.Poll {
                running = false // the service stopped while the poll was held open
                return ClipPush.Poll.Rows(listOf(clip(91, "mcp", "late")), head = 91)
            }
        }
        val copied = mutableListOf<String>()
        val cursor = Cursor(90)
        ClipPush.Loop(server::poll, { cursor.value }, { cursor.value = it }, { copied += it }, { running }).step()
        assertTrue("a poll completing after stop copied $copied", copied.isEmpty())
        assertEquals(90L, cursor.value)
    }

    @Test
    fun stopEndsTheListenerThreadAndNoFurtherPollStarts() {
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        var polls = 0
        val copied = mutableListOf<String>()
        val listener = ClipPushListener(
            enabled = { true },
            poll = { _, _, _ ->
                polls++
                entered.countDown()
                release.await(5, TimeUnit.SECONDS)
                ClipPush.Poll.Rows(listOf(clip(201, "mcp", "after stop")), head = 201)
            },
            loadCursor = { 200L },
            saveCursor = { },
            copy = { copied += it },
            sleeper = { },
        )
        listener.start()
        assertTrue(entered.await(5, TimeUnit.SECONDS))
        listener.stop()
        assertFalse(listener.isRunning)
        release.countDown()
        Thread.sleep(300)
        assertTrue("copied after stop: $copied", copied.isEmpty())
        assertEquals("a poll started after stop", 1, polls)
    }

    @Test
    fun theServiceStopsTheListenerWhenItIsDestroyed() {
        org.robolectric.shadows.ShadowSettings.setCanDrawOverlays(true)
        val entered = CountDownLatch(1)
        val listener = ClipPushListener(
            enabled = { true },
            poll = { _, _, _ -> entered.countDown(); Thread.sleep(50); ClipPush.Poll.Rows(emptyList(), 1_000) },
            loadCursor = { 1_000L },
            saveCursor = { },
            copy = { },
            sleeper = { Thread.sleep(it) },
        )
        val c = Robolectric.buildService(BubbleService::class.java)
        c.get().clipPushFactory = { listener }
        c.create()
        assertTrue("the service never started the listener", entered.await(5, TimeUnit.SECONDS))
        assertTrue(listener.isRunning)
        c.destroy()
        assertFalse("onDestroy left the agent push running", listener.isRunning)
    }

    private fun idleListener(enabled: () -> Boolean = { true }) = ClipPushListener(
        enabled = enabled,
        poll = { _, _, _ -> Thread.sleep(20); ClipPush.Poll.Rows(emptyList(), 1_000) },
        loadCursor = { 1_000L }, saveCursor = { }, copy = { },
        sleeper = { Thread.sleep(it.coerceAtMost(50)) },
    )

    private fun serviceWith(listener: ClipPushListener, interactive: Boolean): org.robolectric.android.controller.ServiceController<BubbleService> {
        org.robolectric.shadows.ShadowSettings.setCanDrawOverlays(true)
        val pm = RuntimeEnvironment.getApplication().getSystemService(android.os.PowerManager::class.java)
        shadowOf(pm).setIsInteractive(interactive)
        val c = Robolectric.buildService(BubbleService::class.java)
        c.get().clipPushFactory = { listener }
        c.create()
        return c
    }

    @Test
    fun aServiceStartedWithTheScreenOffStopsThePushAfterTheGrace() {
        // Round 1 🟡5: onCreate started the listener with no screen check.
        val listener = idleListener()
        val c = serviceWith(listener, interactive = false)
        assertTrue(listener.isRunning)
        shadowOf(android.os.Looper.getMainLooper()).idleFor(
            java.time.Duration.ofMillis(ClipPushListener.SCREEN_OFF_GRACE_MS + 1_000)
        )
        assertFalse("screen off since start, and the push still runs", listener.isRunning)
        c.destroy()
    }

    @Test
    fun aServiceStartedWithTheScreenOnKeepsThePushRunning() {
        val listener = idleListener()
        val c = serviceWith(listener, interactive = true)
        shadowOf(android.os.Looper.getMainLooper()).idleFor(
            java.time.Duration.ofMillis(ClipPushListener.SCREEN_OFF_GRACE_MS + 1_000)
        )
        assertTrue(listener.isRunning)
        c.destroy()
    }

    @Test
    fun turningTheSettingOffStopsThePushNowAndOnStartsItAgain() {
        // 🟢: the toggle used to wait for the next screen-on or bubble start.
        val ctx = RuntimeEnvironment.getApplication()
        val prefs = SttSettings.prefs(ctx)
        val settings = ClipSettings(prefs, TokenStore(prefs, object : SecretCipher {
            override fun encrypt(plaintext: String) = plaintext
            override fun decrypt(blob: String): String? = blob
        }, ClipSettings.KEY_TOKEN_CIPHERTEXT))
        val listener = idleListener(enabled = { settings.agentPush })
        val c = serviceWith(listener, interactive = true)
        assertTrue(listener.isRunning)
        settings.agentPush = false
        shadowOf(android.os.Looper.getMainLooper()).idle()
        assertFalse("the push kept running after the user turned it off", listener.isRunning)
        settings.agentPush = true
        shadowOf(android.os.Looper.getMainLooper()).idle()
        assertTrue("turning it back on did not start it", listener.isRunning)
        c.destroy()
    }

    @Test
    fun aDisabledOrUnpairedListenerNeverPolls() {
        var polls = 0
        val listener = ClipPushListener(
            enabled = { false },
            poll = { _, _, _ -> polls++; ClipPush.Poll.Failed("x") },
            loadCursor = { 1L }, saveCursor = { }, copy = { },
        )
        listener.start()
        Thread.sleep(100)
        assertEquals(0, polls)
        assertFalse(listener.isRunning)
    }

    @Test
    fun theFeedAnswerIsParsedAndAPreFeedServerIsAFailure() {
        val ok = ClipPush.parse(
            """{"clips":[{"id":4,"seq":12,"device":"mcp","body":"hi","created_at":1}],"limit":100,"head":12}"""
        )
        assertEquals(ClipPush.Poll.Rows(listOf(clip(12, "mcp", "hi")), 12), ok)
        assertTrue(ClipPush.parse("""{"clips":[],"limit":100}""") is ClipPush.Poll.Failed)
        assertTrue(ClipPush.parse("""{"clips":[{"id":4,"device":"mcp","body":"x"}],"head":3}""") is ClipPush.Poll.Failed)
    }

    @Test
    fun theSettingIsOnByDefaultAndShownInSettings() {
        val ctx = RuntimeEnvironment.getApplication()
        val p = ctx.getSharedPreferences("push-default", Context.MODE_PRIVATE)
        assertTrue(ClipSettings(p, TokenStore(p, object : SecretCipher {
            override fun encrypt(plaintext: String) = plaintext
            override fun decrypt(blob: String): String? = blob
        })).agentPush)
        val c = Robolectric.buildActivity(SettingsActivity::class.java).setup()
        assertTrue(c.get().findViewById<CheckBox>(R.id.clip_agent_push).isChecked)
        c.destroy()
    }
}
