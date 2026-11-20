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

    @Test
    fun onlyMcpClipsAreCopied() {
        val server = Server(
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
        loop(server, cursor, copied).step()
        assertEquals(listOf("from the agent", "second agent text"), copied)
        assertEquals("the cursor is the max seq seen, copied or not", 735L, cursor.value)
    }

    @Test
    fun theCursorIsPersistedAndTheNextPollStartsFromIt() {
        val server = Server(
            ClipPush.Poll.Rows(listOf(clip(412, "pixel-8", "a"), clip(415, "mcp", "b")), head = 415),
        )
        val cursor = Cursor(407)
        val copied = mutableListOf<String>()
        loop(server, cursor, copied).step()
        // A NEW loop (a restart) over the same persisted cursor.
        loop(server, cursor, copied).step()
        assertEquals(Triple(407L, ClipPush.WAIT_S, ClipPush.LIMIT), server.calls[0])
        assertEquals("the restart did not resume at the persisted cursor", 415L, server.calls[1].first)
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
            ClipPush.Poll.Failed("x"), ClipPush.Poll.Failed("x"), ClipPush.Poll.Failed("x"),
            ClipPush.Poll.Rows(emptyList(), head = 77), ClipPush.Poll.Failed("x"),
        )
        val l = loop(server, Cursor(77), mutableListOf())
        assertEquals(listOf(2_000L, 4_000L, 8_000L), List(3) { l.step() })
        assertEquals("an empty answer pauses briefly, never busy-polls", ClipPush.EMPTY_PAUSE_MS, l.step())
        assertEquals("a success resets the backoff", 2_000L, l.step())
        assertEquals(ClipPush.BACKOFF_MAX_MS, ClipPush.backoffMs(40))
        assertTrue(ClipPush.backoffMs(8) <= ClipPush.BACKOFF_MAX_MS)
    }

    @Test
    fun tooManyWaitersHonoursRetryAfterWithinBounds() {
        val l = loop(Server(ClipPush.Poll.RetryAfter(7_000), ClipPush.Poll.RetryAfter(86_400_000)), Cursor(5), mutableListOf())
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
