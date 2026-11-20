package dev.zacx.promptshipper

import android.widget.Button
import android.widget.TextView
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config

/**
 * The main screen's Server row: an unpaired app says so and offers pairing;
 * a paired one names its transcription host. There is no default server, so
 * this row is the first-run entry point.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class MainServerRowTest {

    private val ctx get() = RuntimeEnvironment.getApplication()

    @Test
    fun anUnpairedAppSaysSoAndThePairButtonOpensPairing() {
        val c = Robolectric.buildActivity(MainActivity::class.java).setup()
        val a = c.get()
        assertEquals(
            ctx.getString(R.string.server_unpaired),
            a.findViewById<TextView>(R.id.server_status).text.toString(),
        )
        val pair = a.findViewById<Button>(R.id.pair_button)
        assertEquals(ctx.getString(R.string.pair_button), pair.text.toString())
        pair.performClick()
        assertEquals(
            PairingActivity::class.java.name,
            shadowOf(a).nextStartedActivity.component?.className,
        )
        c.destroy()
    }

    @Test
    fun aPairedAppNamesItsTranscriptionHost() {
        SttSettings.create(ctx).baseUrl = "http://100.101.102.103:8118"
        val c = Robolectric.buildActivity(MainActivity::class.java).setup()
        val a = c.get()
        assertEquals(
            ctx.getString(R.string.server_paired, "100.101.102.103"),
            a.findViewById<TextView>(R.id.server_status).text.toString(),
        )
        assertEquals(
            ctx.getString(R.string.repair_button),
            a.findViewById<Button>(R.id.pair_button).text.toString(),
        )
        c.destroy()
    }
}
