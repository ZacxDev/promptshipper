package dev.zacx.promptshipper

import android.content.Context
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config

/**
 * The consumer half of `docs/pairing.md` (v1). Robolectric only for
 * `org.json`, which is the platform's.
 *
 * The fixture tokens are distinct from each other and from every constant
 * the code names, so an implementation that swapped the two servers, or
 * stored one token twice, fails.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class PairingPayloadTest {

    private val sttToken = "3f9a0c" + "1".repeat(58)
    private val clipToken = "b7e21d" + "2".repeat(58)

    /** Exactly the shape `./setup` emits (tests/test_setup.py pins it there). */
    private fun payload(
        v: String = "1",
        type: String = "\"promptshipper-pairing\"",
        name: String? = "\"my-desktop\"",
        stt: String = """{"url":"http://100.101.102.103:8118","token":"$sttToken"}""",
        clip: String = """{"url":"http://100.101.102.103:8139","token":"$clipToken"}""",
        extra: String = "",
    ): String = buildString {
        append("{\"v\":$v,\"type\":$type,")
        if (name != null) append("\"name\":$name,")
        append("\"stt\":$stt,\"clip\":$clip$extra}")
    }

    private fun ok(text: String): PairingPayload.Pairing {
        val r = PairingPayload.parse(text)
        assertTrue("expected Ok, got $r", r is PairingPayload.Result.Ok)
        return (r as PairingPayload.Result.Ok).pairing
    }

    private fun rejected(text: String): String {
        val r = PairingPayload.parse(text)
        assertTrue("expected Rejected, got $r", r is PairingPayload.Result.Rejected)
        return (r as PairingPayload.Result.Rejected).reason
    }

    @Test
    fun theSetupPayloadParsesIntoBothServers() {
        val p = ok(payload())
        assertEquals("my-desktop", p.name)
        assertEquals("http://100.101.102.103:8118", p.stt.url)
        assertEquals(sttToken, p.stt.token)
        assertEquals("http://100.101.102.103:8139", p.clip.url)
        assertEquals(clipToken, p.clip.token)
    }

    @Test
    fun surroundingWhitespaceFromAScannerOrAPasteIsTolerated() {
        assertEquals(sttToken, ok("\n  " + payload() + "\n").stt.token)
    }

    @Test
    fun unknownFieldsAreIgnoredAtEveryLevel() {
        val p = ok(
            payload(
                stt = """{"url":"http://100.101.102.103:8118","token":"$sttToken","x":1}""",
                extra = ""","mcp":{"url":"http://100.101.102.103:8140"}""",
            )
        )
        assertEquals(sttToken, p.stt.token)
    }

    @Test
    fun aDifferentVersionIsRejectedWithAnUpdateTheAppMessage() {
        assertTrue(rejected(payload(v = "2")).contains("update promptshipper"))
        assertTrue(rejected(payload(v = "0")).contains("version 0"))
        // Never best-effort: a non-integer version is malformed, not "1-ish".
        assertTrue(rejected(payload(v = "\"1\"")).contains("\"v\""))
        assertTrue(rejected(payload(v = "1.0")).contains("\"v\""))
        assertTrue(rejected(payload().replace("\"v\":1,", "")).contains("\"v\""))
    }

    @Test
    fun anUnrelatedQrCodeIsNotOurs() {
        for (t in listOf(
            "https://example.com/menu",
            "WIFI:S:home;T:WPA;P:secret;;",
            """{"hello":"world"}""",
            payload(type = "\"something-else\""),
            "[1,2,3]",
        )) {
            assertEquals(t, "it is not a promptshipper pairing code", rejected(t))
        }
        assertEquals("it is empty", rejected("   "))
        assertEquals("it is not valid JSON", rejected("{\"v\":1,"))
    }

    @Test
    fun textAfterTheObjectIsRefused() {
        assertTrue(rejected(payload() + " trailing").contains("after the JSON"))
    }

    @Test
    fun halfAServerIsNeverPaired() {
        // docs/pairing.md: "the app must not pair half a server silently".
        val noClip = payload().substringBefore(",\"clip\"") + "}"
        assertTrue(rejected(noClip).contains("\"clip\""))
        assertTrue(rejected(payload(stt = """{"url":"http://100.101.102.103:8118"}""")).contains("token"))
        assertTrue(rejected(payload(clip = """{"token":"$clipToken"}""")).contains("url"))
        assertTrue(rejected(payload(stt = "\"http://100.101.102.103:8118\"")).contains("\"stt\""))
    }

    @Test
    fun theUrlRulesApply() {
        // A path breaks every request the client builds.
        assertTrue(
            rejected(payload(stt = """{"url":"http://100.101.102.103:8118/v1","token":"$sttToken"}"""))
                .contains("no path"),
        )
        // Cleartext to a public address or a hostname: https only.
        assertTrue(
            rejected(payload(clip = """{"url":"http://203.0.113.5:8139","token":"$clipToken"}"""))
                .contains("private IP"),
        )
        assertTrue(
            rejected(payload(stt = """{"url":"http://desk.tail1234.ts.net:8118","token":"$sttToken"}"""))
                .contains("private IP"),
        )
        // ...and https to a name is fine; so is a bracketed IPv6 ULA.
        assertEquals(
            "https://desk.tail1234.ts.net",
            ok(payload(stt = """{"url":"https://desk.tail1234.ts.net","token":"$sttToken"}""")).stt.url,
        )
        assertEquals(
            "http://[fd7a:115c:a1e0::1]:8139",
            ok(payload(clip = """{"url":"http://[fd7a:115c:a1e0::1]:8139","token":"$clipToken"}""")).clip.url,
        )
    }

    @Test
    fun theTokenRulesApplyAndTheTokenIsTakenVerbatim() {
        val shortest = "A".repeat(23) + "~"
        assertEquals(24, shortest.length)
        assertEquals(shortest, ok(payload(stt = """{"url":"http://10.0.0.2:8118","token":"$shortest"}""")).stt.token)
        val tooShort = "a".repeat(23)
        assertTrue(rejected(payload(stt = """{"url":"http://10.0.0.2:8118","token":"$tooShort"}""")).contains("token"))
        // A space would be "trimmed" by a sloppy client; it is refused, never altered.
        assertTrue(rejected(payload(clip = """{"url":"http://10.0.0.2:8139","token":" $clipToken"}""")).contains("token"))
        assertTrue(rejected(payload(clip = """{"url":"http://10.0.0.2:8139","token":"${clipToken}/"}""")).contains("token"))
    }

    @Test
    fun noRejectionQuotesAToken() {
        val reasons = listOf(
            rejected(payload(v = "2")),
            rejected(payload(clip = """{"url":"http://203.0.113.5:8139","token":"$clipToken"}""")),
            rejected(payload(stt = """{"url":"http://10.0.0.2:8118","token":"${sttToken}!"}""")),
        )
        for (r in reasons) {
            assertFalse(r, r.contains(sttToken.take(12)) || r.contains(clipToken.take(12)))
        }
        assertFalse(ok(payload()).toString().contains(sttToken))
    }

    @Test
    fun aMissingNameFallsBackToTheSttHost() {
        assertNull(ok(payload(name = null)).name)
        assertEquals("100.101.102.103", ok(payload(name = null)).displayName)
        assertEquals("my-desktop", ok(payload()).displayName)
    }

    // --- PairingWriter -------------------------------------------------------

    private val ctx: Context get() = RuntimeEnvironment.getApplication()

    private object Identity : SecretCipher {
        override fun encrypt(plaintext: String): String = "e:$plaintext"
        override fun decrypt(blob: String): String? = blob.removePrefix("e:")
    }

    /** Encrypts the first [okCalls] values, then throws. */
    private class FailingAfter(private var okCalls: Int) : SecretCipher {
        override fun encrypt(plaintext: String): String {
            if (okCalls-- <= 0) throw IllegalStateException("keystore gone")
            return "e:$plaintext"
        }
        override fun decrypt(blob: String): String? = blob.removePrefix("e:")
    }

    private fun stt(cipher: SecretCipher = Identity): SttSettings {
        val p = ctx.getSharedPreferences("pairing_test", Context.MODE_PRIVATE)
        return SttSettings(p, TokenStore(p, cipher))
    }

    private fun clip(cipher: SecretCipher = Identity): ClipSettings {
        val p = ctx.getSharedPreferences("pairing_test", Context.MODE_PRIVATE)
        return ClipSettings(p, TokenStore(p, cipher, ClipSettings.KEY_TOKEN_CIPHERTEXT))
    }

    @Test
    fun withNoServerATokenAloneIsNotARemoteConfiguration() {
        // 🔴 dicta defaulted to a hardcoded server address, so a token alone
        // meant "upload there". promptshipper has no default: no URL, no upload.
        val s = stt()
        s.tokens.putToken(sttToken)
        assertNull(s.baseUrl)
        assertFalse(s.isRemoteConfigured)
        s.baseUrl = "http://100.101.102.103:8118"
        assertTrue(s.isRemoteConfigured)
    }

    @Test
    fun applyingAPairingConfiguresBothServicesWithSeparateTokens() {
        assertFalse(PairingWriter.isPaired(stt()))
        stt().remoteEnabled = false
        PairingWriter.apply(ok(payload()), stt(), clip())
        assertTrue(PairingWriter.isPaired(stt()))
        assertEquals("http://100.101.102.103:8118", stt().baseUrl)
        assertEquals(sttToken, stt().tokens.token())
        assertEquals("http://100.101.102.103:8139", clip().baseUrl)
        assertEquals(clipToken, clip().tokens.token())
        assertTrue("pairing must switch remote transcription on", stt().isRemoteConfigured)
        assertTrue(clip().isConfigured)
    }

    @Test
    fun aFailedWriteRollsEverythingBack() {
        // An earlier pairing is in place.
        val oldStt = "0ld5tt" + "9".repeat(30)
        val oldClip = "0ldc1p" + "8".repeat(30)
        stt().baseUrl = "http://10.0.0.7:8118"
        stt().tokens.putToken(oldStt)
        clip().baseUrl = "http://10.0.0.7:8139"
        clip().tokens.putToken(oldClip)

        // The STT token encrypts, the clip token's encryption throws.
        val s = stt(FailingAfter(1))
        val c = clip(FailingAfter(0))
        try {
            PairingWriter.apply(ok(payload()), s, c)
            throw AssertionError("the failed encryption was swallowed")
        } catch (_: IllegalStateException) {
        }
        assertEquals("http://10.0.0.7:8118", stt().baseUrl)
        assertEquals(oldStt, stt().tokens.token())
        assertEquals("http://10.0.0.7:8139", clip().baseUrl)
        assertEquals(oldClip, clip().tokens.token())
    }
}
