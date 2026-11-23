package dev.zacx.promptshipper

import android.content.pm.PackageManager
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import java.io.File

/**
 * The two manifest facts that silently break the whole feature.
 *
 * ## Why these are tests rather than a comment
 *
 * Both failures are **invisible at build time**:
 *
 *  * A missing `android.permission.INTERNET` raises no permission dialog, no
 *    lint error and no build warning. Every socket just throws at runtime,
 *    which looks exactly like the service being down.
 *  * Cleartext HTTP is blocked by default on targetSdk 28+. promptshipper
 *    needs it for a private-network server whose address arrives at pairing
 *    time, so the platform permits it and [PrivateNetwork] restricts it to
 *    private IP literals — a rule that only holds if every connection goes
 *    through it.
 *
 * **Invariant guards**, labelled as such: neither has regressed. They are
 * here because the cost of either regressing is a feature that silently does
 * not work, or a security property silently lost.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class NetworkManifestTest {

    private val ctx get() = RuntimeEnvironment.getApplication()

    @Test
    fun theAppDeclaresTheInternetPermission() {
        val info = ctx.packageManager.getPackageInfo(
            ctx.packageName, PackageManager.GET_PERMISSIONS
        )
        val declared = info.requestedPermissions?.toList().orEmpty()
        assertTrue(
            "android.permission.INTERNET is not declared — every socket will " +
                "throw at runtime with no build-time warning. Declared: $declared",
            declared.contains("android.permission.INTERNET"),
        )
    }

    @Test
    fun theAppStillDeclaresRecordAudio() {
        // Positive control for the test above: proves the permission list is
        // actually being read, so a passing INTERNET assertion is not just an
        // empty list matching nothing. Without this, both could be vacuous.
        val info = ctx.packageManager.getPackageInfo(
            ctx.packageName, PackageManager.GET_PERMISSIONS
        )
        val declared = info.requestedPermissions?.toList().orEmpty()
        assertTrue(
            "the permission list came back without RECORD_AUDIO, so it is not " +
                "being read correctly and the INTERNET check proves nothing",
            declared.contains("android.permission.RECORD_AUDIO"),
        )
    }

    // --- the network security config ---------------------------------------

    /**
     * Reads the source manifest rather than the parsed one: the attributes
     * under test are `android:usesCleartextTraffic` (whose *absence* is the
     * property) and `android:networkSecurityConfig`, and an absent attribute
     * is not observable through `ApplicationInfo` in a way that distinguishes
     * "not set" from "set to the platform default".
     */
    private fun manifestSource(): String =
        withoutComments(
            read(
                "app/src/main/AndroidManifest.xml",
                "src/main/AndroidManifest.xml",
                "../app/src/main/AndroidManifest.xml",
                what = "AndroidManifest.xml",
            )
        )

    private fun securityConfigSource(): String =
        withoutComments(
            read(
                "app/src/main/res/xml/network_security_config.xml",
                "src/main/res/xml/network_security_config.xml",
                "../app/src/main/res/xml/network_security_config.xml",
                what = "network_security_config.xml",
            )
        )

    private fun read(vararg paths: String, what: String): String {
        val found = paths.map { File(it) }.firstOrNull { it.exists() }
        assertTrue(
            "could not locate $what from ${File(".").absolutePath} — refusing " +
                "to report a pass on a file that was never read",
            found != null,
        )
        return found!!.readText()
    }

    /**
     * Strips XML comments before any assertion runs.
     *
     * 🔴 Not cosmetic — this is the difference between a guard on the XML and
     * a guard on the prose. Both files *document* the attribute they must not
     * use (`usesCleartextTraffic`) and the reasoning for the scoped
     * alternative, so a raw substring search finds the word inside the comment
     * that exists to explain why it is absent. The first version of
     * [theManifestDoesNotSetBlanketCleartextTraffic] failed for exactly that
     * reason: it was asserting against a word rather than against
     * configuration, and it would equally have *passed* on a manifest that set
     * the attribute while renaming the comment.
     */
    private fun withoutComments(xml: String): String =
        xml.replace(Regex("<!--.*?-->", RegexOption.DOT_MATCHES_ALL), "")

    @Test
    fun theManifestPointsAtAScopedNetworkSecurityConfig() {
        val manifest = manifestSource()
        assertTrue(
            "the application element must reference the scoped config",
            manifest.contains(
                "android:networkSecurityConfig=\"@xml/network_security_config\""
            ),
        )
    }

    @Test
    fun theManifestDoesNotSetBlanketCleartextTraffic() {
        // One place for the policy: the attribute would be a second, silent
        // switch beside network_security_config.xml.
        val manifest = manifestSource()
        assertFalse(
            "android:usesCleartextTraffic must not be set at all — the XML " +
                "config plus PrivateNetwork is the one place the rule lives",
            manifest.contains("usesCleartextTraffic"),
        )
    }

    /**
     * Negative control for [theManifestDoesNotSetBlanketCleartextTraffic].
     *
     * 🔴 The real assertion is that a substring is **absent**, and an
     * absence is indistinguishable from a check wired to the wrong string or
     * the wrong file. This feeds the same comment-stripping pipeline a
     * manifest that *does* set the attribute, and requires it to be seen —
     * and feeds it one that only mentions it in a comment, and requires it
     * NOT to be seen. Without both halves the clean result above is not
     * evidence of anything.
     */
    @Test
    fun theCleartextCheckSeesTheAttributeButNotAMentionOfItInAComment() {
        val setsIt = """
            <manifest><application
                android:usesCleartextTraffic="true"
                android:label="x" /></manifest>
        """.trimIndent()
        assertTrue(
            "the check cannot detect the attribute when it IS set, so the " +
                "real test's pass proves nothing",
            withoutComments(setsIt).contains("usesCleartextTraffic"),
        )

        val onlyMentionsIt = """
            <manifest>
            <!-- networkSecurityConfig rather than usesCleartextTraffic="true" -->
            <application android:label="x" /></manifest>
        """.trimIndent()
        assertFalse(
            "a mention inside a comment must not be mistaken for the attribute",
            withoutComments(onlyMentionsIt).contains("usesCleartextTraffic"),
        )
    }

    // cleartextIsPermittedForTheMeshHostOnly, exactlyOneDomainIsExempted,
    // noDomainConfigPermitsCleartextForSubdomains and the two
    // ...DefaultBaseUrlPointsAtTheExemptedHost seam guards were REPLACED when
    // promptshipper dropped dicta's hardcoded server. There is no host to name
    // any more: the address arrives at pairing time and the platform config
    // cannot express an address range, so the private-range rule moved into
    // code (PrivateNetwork). The two tests below pin that new arrangement —
    // the config's shape, and that no connection can bypass the code gate.

    @Test
    fun theSecurityConfigDelegatesTheRuleToTheAppAndNamesNoHost() {
        val config = securityConfigSource()
        assertTrue(
            "the base config must permit cleartext — the private-range rule " +
                "is PrivateNetwork's, and a platform refusal would break " +
                "every http:// pairing",
            config.contains("<base-config cleartextTrafficPermitted=\"true\">"),
        )
        assertFalse(
            "a <domain> entry names a host in a public repository and scopes " +
                "nothing the app cannot already decide; there must be none",
            config.contains("<domain"),
        )
    }

    /**
     * 🔴 A SEAM LEDGER, not a component test: [PrivateNetwork] only protects a
     * request that goes through it. The set of main-source files that open a
     * URL connection must be EXACTLY `PrivateNetwork.kt` — a second
     * `openConnection(` anywhere is a request the cleartext rule never sees,
     * and the set shrinking to nothing means this scan is wired to nothing.
     */
    @Test
    fun onlyPrivateNetworkOpensAConnection() {
        val dir = listOf(
            File("src/main/java/dev/zacx/promptshipper"),
            File("app/src/main/java/dev/zacx/promptshipper"),
        ).firstOrNull { it.isDirectory }
        assertTrue("main sources not found from ${File(".").absolutePath}", dir != null)
        val openers = dir!!.walk().filter { it.isFile && it.extension == "kt" }
            .filter {
                SourceText.withoutComments(SourceText.withoutLiterals(it.readText()))
                    .contains("openConnection(")
            }
            .map { it.name }
            .toSet()
        assertEquals(setOf("PrivateNetwork.kt"), openers)
        for (client in listOf("SttHttp.kt", "ClipHttp.kt", "SnipSync.kt")) {
            assertTrue(
                "$client's default opener is not PrivateNetwork::open",
                File(dir, client).readText().contains("= PrivateNetwork::open"),
            )
        }
    }
}
