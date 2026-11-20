package dev.zacx.promptshipper

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/**
 * [PrivateNetwork]: plain `http://` only to a private IP literal.
 *
 * Every range is tested at its two edges AND one address just outside each
 * edge, because a range check is wrong at a boundary or not at all
 * (`172.16/12` is the one people get wrong: it ends at `172.31`, not
 * `172.255`).
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34]) // ClipHttp builds its body with the platform's org.json.
class PrivateNetworkTest {

    @Test
    fun privateIpv4RangesAreAllowedAtBothEdges() {
        for (h in listOf(
            "10.0.0.0", "10.255.255.255",
            "172.16.0.0", "172.31.255.255",
            "192.168.0.0", "192.168.255.255",
            "100.64.0.0", "100.127.255.255",
            "127.0.0.1", "127.255.255.255",
            "localhost", "LOCALHOST",
        )) {
            assertTrue("$h should be private", PrivateNetwork.isPrivateHost(h))
        }
    }

    @Test
    fun addressesJustOutsideEachRangeArePublic() {
        for (h in listOf(
            "9.255.255.255", "11.0.0.0",
            "172.15.255.255", "172.32.0.0",
            "192.167.255.255", "192.169.0.0",
            "100.63.255.255", "100.128.0.0",
            "126.255.255.255", "128.0.0.0",
            "8.8.8.8", "203.0.113.5", "0.0.0.0", "169.254.1.1",
        )) {
            assertFalse("$h should be public", PrivateNetwork.isPrivateHost(h))
        }
    }

    @Test
    fun ipv6LoopbackAndUniqueLocalAreAllowedAndNothingElse() {
        for (h in listOf("::1", "0:0:0:0:0:0:0:1", "fd00::", "fd7a:115c:a1e0::1",
            "FD7A:115C:A1E0:ab12:4843:cd96:6258:b240", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")) {
            assertTrue("$h should be private", PrivateNetwork.isPrivateHost(h))
        }
        for (h in listOf("::", "::2", "fc00::1", "fe80::1", "2001:db8::1", "fe00::",
            "fd00::1%wlan0", "::ffff:10.0.0.1", "1:2:3:4:5:6:7:8:9", "fd00:::1")) {
            assertFalse("$h should not be private", PrivateNetwork.isPrivateHost(h))
        }
    }

    @Test
    fun aHostnameIsNeverPrivateWhateverItLooksLike() {
        // 🔴 The name is resolved AFTER this check, so a name cannot be judged.
        for (h in listOf("desktop.tail1234.ts.net", "desktop", "nas.local", "box.lan",
            "10.0.0.1.example.com", "192.168.1.1.nip.io", "localhost.example")) {
            assertFalse("$h is a hostname", PrivateNetwork.isPrivateHost(h))
        }
    }

    @Test
    fun ambiguousIpv4SpellingsAreRefusedNotGuessed() {
        for (h in listOf("010.0.0.1", "10.0.0", "10.0.0.0.1", "10.0.0.256", "10.0.0.-1",
            "1e1.0.0.1", " 10.0.0.1")) {
            assertNull("$h parsed as an address", PrivateNetwork.ipv4(h))
        }
    }

    @Test
    fun cleartextRejectionJudgesSchemeAndHost() {
        assertNull(PrivateNetwork.cleartextRejection("http://100.101.102.103:8118"))
        assertNull(PrivateNetwork.cleartextRejection("http://[fd7a:115c:a1e0::1]:8118"))
        assertNull(PrivateNetwork.cleartextRejection("https://stt.example.com"))
        assertNull(PrivateNetwork.cleartextRejection("https://203.0.113.5:8118"))
        assertNotNull(PrivateNetwork.cleartextRejection("http://203.0.113.5:8118"))
        assertNotNull(PrivateNetwork.cleartextRejection("http://desktop.tail1234.ts.net:8118"))
        assertNotNull(PrivateNetwork.cleartextRejection("ftp://10.0.0.1"))
        // userinfo is not the host: the request would go to 203.0.113.5.
        assertNotNull(PrivateNetwork.cleartextRejection("http://10.0.0.1@203.0.113.5:8118"))
        assertEquals("203.0.113.5", PrivateNetwork.hostOf("http://10.0.0.1@203.0.113.5:8118/x"))
    }

    @Test
    fun openRefusesCleartextToAPublicHostWithoutConnecting() {
        // The production factory of both HTTP clients. An IOException, so
        // both clients report it as "could not reach the server".
        try {
            PrivateNetwork.open("http://203.0.113.5:8118/healthz")
            throw AssertionError("cleartext to a public host was opened")
        } catch (e: IOException) {
            assertTrue(e.message!!, e.message!!.startsWith("refused:"))
        }
    }

    @Test
    fun openDoesNotFollowRedirects() {
        // openConnection() does not connect, so this needs no server.
        val conn: HttpURLConnection = PrivateNetwork.open("http://127.0.0.1:9/healthz")
        assertFalse(
            "a private host's 302 to a public http:// URL would be followed in " +
                "cleartext, bearer token and all",
            conn.instanceFollowRedirects,
        )
        // Control: the platform default the factory overrides is `true`, so
        // the assertion above is about the factory, not the JVM.
        assertTrue((URL("http://127.0.0.1:9/").openConnection() as HttpURLConnection).instanceFollowRedirects)
    }

    @Test
    fun bothClientsRefuseAPublicCleartextUrlThroughTheirDefaultOpener() {
        // Behavioural half of NetworkManifestTest.onlyPrivateNetworkOpensAConnection:
        // the default opener really is the gate, end to end, on both clients.
        // transcribe, not healthz: healthz returns false for a refusal AND for
        // an unreachable address, so it could not tell the gate from no gate.
        val stt = SttHttp("http://203.0.113.5:8118", token = "t".repeat(32)).transcribe(
            TranscriptionCoordinator.UploadAttempt(
                audio = ByteArray(16), audioSeconds = 1.0, language = "en", attemptNumber = 1,
            )
        )
        assertTrue("$stt", stt is SttOutcome.Network)
        assertTrue("$stt", (stt as SttOutcome.Network).reason.startsWith("refused:"))
        val post = ClipHttp("http://203.0.113.5:8139", "t".repeat(32)).post("x", "dev")
        assertTrue("$post", post is ClipPost.Unreachable)
        assertTrue("$post", (post as ClipPost.Unreachable).reason.startsWith("refused:"))
    }

    @Test
    fun theBaseUrlRuleAppliesTheCleartextRuleAfterTheShapeRule() {
        assertNull(BaseUrlRule.rejectionFor("", "/v1/clips"))
        assertNull(BaseUrlRule.rejectionFor("http://192.168.1.20:8139", "/v1/clips"))
        assertNull(BaseUrlRule.rejectionFor("https://clips.example.com", "/v1/clips"))
        assertTrue(
            BaseUrlRule.rejectionFor("http://192.168.1.20:8139/v1/clips", "/v1/clips")!!
                .contains("appends /v1/clips"),
        )
        assertTrue(
            BaseUrlRule.rejectionFor("http://clips.example.com", "/v1/clips")!!
                .contains("private IP"),
        )
        assertNotNull(BaseUrlRule.rejectionFor("http://user:pw@192.168.1.20:8139", "/v1/clips"))
        assertTrue(
            SttEndpoints.rejectionFor("http://10.0.0.5:8118/v1/audio/transcriptions")!!
                .contains("appends /v1/audio/transcriptions"),
        )
    }
}
