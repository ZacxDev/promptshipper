package dev.zacx.promptshipper

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

/**
 * [BubbleDictation.Event.CloseTapped] — the decisions, with no Android in sight.
 *
 * [BubbleClipStateTest]'s file, applied to the fourth reveal button: the one
 * that turns the bubble off. The wiring (that a view raises it, that the shell
 * stops the service) is [BubbleCloseRevealTest]'s and needs a real service.
 *
 * NEW BEHAVIOUR: [closeTappedInIdleStopsTheBubbleAndNothingElse] and
 * [closeTappedPutsTheRevealAwayBeforeStopping] — the event and its effect did
 * not exist before this change, so both are red on the pre-change tree (as a
 * compile failure: there is no `CloseTapped` to send).
 *
 * 🔴 INVARIANT GUARDS, not regression coverage: the busy-state refusals, the
 * no-microphone claim and the state-unchanged claim. No caller can reach the
 * event outside IDLE today; they pin the machine, not the wiring.
 */
class BubbleCloseStateTest {

    private fun idleMachine() = BubbleDictation { 0L }

    @Test
    fun closeTappedInIdleStopsTheBubbleAndNothingElse() {
        assertEquals(
            listOf(BubbleDictation.Effect.StopBubble),
            idleMachine().on(BubbleDictation.Event.CloseTapped),
        )
    }

    @Test
    fun closeTappedPutsTheRevealAwayBeforeStopping() {
        // ORDER is the assertion: the reveal (and its device-wide
        // FLAG_WATCH_OUTSIDE_TOUCH) goes away first, then the service stops.
        val machine = idleMachine()
        assertEquals(
            "precondition: the long press is what reveals the buttons",
            listOf(BubbleDictation.Effect.ShowActions),
            machine.on(BubbleDictation.Event.BubbleLongPressed),
        )
        assertEquals(
            listOf(
                BubbleDictation.Effect.HideActions,
                BubbleDictation.Effect.StopBubble,
            ),
            machine.on(BubbleDictation.Event.CloseTapped),
        )
    }

    @Test
    fun closeTappedInEveryBusyStateDoesNotStopTheBubble() {
        for (state in listOf(
            BubbleDictation.State.RECORDING,
            BubbleDictation.State.TRANSCRIBING,
        )) {
            assertEquals(
                "a close tap in $state would tear the service down under a " +
                    "live microphone or an in-flight upload",
                emptyList<BubbleDictation.Effect>(),
                machineIn(state).on(BubbleDictation.Event.CloseTapped),
            )
        }
    }

    @Test
    fun closeTappedNeverReachesAMicrophonePathInAnyState() {
        for (state in BubbleDictation.State.values()) {
            val effects = machineIn(state).on(BubbleDictation.Event.CloseTapped)
            assertFalse(
                "a close tap in $state emitted StartRecording: $effects",
                effects.contains(BubbleDictation.Effect.StartRecording),
            )
            assertFalse(
                "a close tap in $state emitted RequestMicPermission, which " +
                    "opens the card: $effects",
                effects.contains(BubbleDictation.Effect.RequestMicPermission),
            )
        }
    }

    @Test
    fun closeTappedLeavesTheStateExactlyAsItWas() {
        for (state in BubbleDictation.State.values()) {
            val machine = machineIn(state)
            machine.on(BubbleDictation.Event.CloseTapped)
            assertEquals("a close tap moved the machine out of $state", state, machine.state)
        }
    }

    @Test
    fun theFourRevealEventsEmitFOURDIFFERENTEffects() {
        // 🔴 A RELATIONSHIP: a copied arm that kept a sibling's effect would be
        // a close button that opens a picker, or a picker button that turns the
        // bubble off. No single-arm test can see that. Fails if any two agree.
        val effects = listOf(
            BubbleDictation.Event.HistoryTapped,
            BubbleDictation.Event.SnipTapped,
            BubbleDictation.Event.ClipTapped,
            BubbleDictation.Event.CloseTapped,
        ).map { idleMachine().on(it).single() }

        assertEquals(
            listOf(
                BubbleDictation.Effect.OpenHistoryPicker,
                BubbleDictation.Effect.OpenSnipPicker,
                BubbleDictation.Effect.OpenClipPicker,
                BubbleDictation.Effect.StopBubble,
            ),
            effects,
        )
        assertEquals(4, effects.toSet().size)
    }
}

/** A machine driven into [state] through its own real transitions. */
private fun machineIn(state: BubbleDictation.State): BubbleDictation {
    var now = 0L
    val machine = BubbleDictation { now }
    when (state) {
        BubbleDictation.State.IDLE -> Unit
        BubbleDictation.State.RECORDING ->
            machine.on(BubbleDictation.Event.BubbleTapped(micGranted = true))
        BubbleDictation.State.TRANSCRIBING -> {
            machine.on(BubbleDictation.Event.BubbleTapped(micGranted = true))
            now = BubbleDictation.MIN_RECORDING_MS + 1
            machine.on(BubbleDictation.Event.BubbleTapped(micGranted = true))
        }
    }
    assertEquals("the fixture did not reach $state", state, machine.state)
    return machine
}
