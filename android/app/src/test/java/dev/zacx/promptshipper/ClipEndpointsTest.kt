package dev.zacx.promptshipper

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The clip store's URLs and auth header, against **literal** expected strings.
 *
 * 🔴 **Invariant guards / characterization tests, NOT regression coverage.**
 * [ClipEndpoints] is a new file, so there is no pre-change tree for these to be
 * red against. They are here for the reason `SttEndpoints.kt:5-10` gives for
 * that object existing at all: the path and the header shape are the two things
 * easy to get subtly wrong, and a wrong path is a 404 that reads like an outage
 * while a re-cased header is a 401 that reads like a bad token.
 *
 * 🔴 **Every expectation below is a literal, never derived from the
 * implementation.** A test that computed the expected URL by calling the same
 * `join` would pass for any `join`. The literals were checked against the live
 * service with `curl` on 2026-11-15 — `/healthz` 200, `GET /v1/clips?limit=3`
 * 200, `POST /v1/clips` 201 then 200 with `deduped:true`, `DELETE
 * /v1/clips/{id}` 204 then 404 — which is what makes them a contract rather
 * than a guess.
 *
 * 🔴 **No token appears anywhere in this file.** `ZacxDev/dicta` is a PUBLIC
 * repository; the fixture below is a random hex string of the right shape.
 */
class ClipEndpointsTest {

    /** Realistic in SHAPE only. Not a credential, and not the operator's. */
    private val fixtureToken =
        "4b1d7e0a93c6f2581ad4ebb70c35f96284e1d0ac57b3629fd8a4c1e07b5392da"

    // theDefaultBaseUrlIsTheMeshHostOnTheClipStoresPort and
    // theDefaultBaseUrlIsNotTheTranscriptionServices were DELETED with the
    // default itself: promptshipper ships knowing no server (see
    // ClipSettingsTest.anUnconfiguredClipStoreHasNoBaseUrlAndIsNotConfigured).

    @Test
    fun theClipsPathsAreTheLiteralsTheServiceServes() {
        assertEquals("/v1/clips", ClipEndpoints.CLIPS_PATH)
        assertEquals(
            "http://100.101.102.103:8139/v1/clips",
            ClipEndpoints.clips("http://100.101.102.103:8139"),
        )
        assertEquals(
            "http://100.101.102.103:8139/v1/clips?limit=25",
            ClipEndpoints.clips("http://100.101.102.103:8139", 25),
        )
        assertEquals(
            "http://100.101.102.103:8139/v1/clips/41",
            ClipEndpoints.clip("http://100.101.102.103:8139", 41L),
        )
    }

    @Test
    fun aTrailingSlashOnTheOperatorsBaseUrlDoesNotDoubleUp() {
        // The base URL is typed by a human into a settings field, so a
        // trailing slash is expected rather than exceptional — and
        // `.../8139//v1/clips` is a path the service's router does not match.
        // Surrounding whitespace comes free with a paste.
        assertEquals(
            "http://100.101.102.103:8139/v1/clips",
            ClipEndpoints.clips("  http://100.101.102.103:8139/  "),
        )
        assertEquals(
            "http://100.101.102.103:8139/v1/clips?limit=7",
            ClipEndpoints.clips("http://100.101.102.103:8139///", 7),
        )
        assertEquals(
            "http://100.101.102.103:8139/v1/clips/3",
            ClipEndpoints.clip("http://100.101.102.103:8139/", 3L),
        )
    }

    @Test
    fun theAuthorizationHeaderIsSttEndpointsOwnAndNotASecondSpelling() {
        // 🔴 One rule, one place. The clip client reuses
        // SttEndpoints.authorization deliberately — the service compares the
        // remainder of the header byte-for-byte against its configured token,
        // so a second copy of this one-line function is a second chance to trim
        // or re-case a token into a 401 that reads like an outage.
        //
        // The literal is written out rather than built from the constant, so
        // this is a claim about the string and not about the function.
        assertEquals(
            "Bearer 4b1d7e0a93c6f2581ad4ebb70c35f96284e1d0ac57b3629fd8a4c1e07b5392da",
            SttEndpoints.authorization(fixtureToken),
        )
    }

    // --- the base-URL validator ----------------------------------------------

    @Test
    fun anAcceptableBaseUrlIsNOTRejected() {
        // 🔴 THE DIRECTION THAT MATTERS FIRST. A validator that refused
        // everything would make every case below pass while bricking the
        // settings screen, and the default is the value the field ships with.
        for (good in listOf(
            "",
            "http://100.101.102.103:8139",
            "http://192.168.1.20:8139",
            "http://[fd7a:115c:a1e0::1]:8139",
            "http://100.101.102.103:8139/",
            "  http://100.101.102.103:8139  ",
            "https://clips.example.test",
            "https://clipstore",
        )) {
            assertEquals(
                "the settings field refuses [$good], which is a usable base URL",
                null,
                ClipEndpoints.rejectionFor(good),
            )
        }
    }

    @Test
    fun theFullENDPOINTIsRejectedRatherThanSilentlyDoubled() {
        // 🔴 The most likely wrong paste there is: the URL out of a curl line.
        // ClipEndpoints.clips appends /v1/clips itself, so accepting this
        // yields `…/v1/clips/v1/clips` — a 404 on every POST, which until this
        // branch DELETED the capture.
        //
        // Rejected and REPORTED, never normalised: a field that quietly edits
        // what the operator typed is how they come to believe a setting says
        // something it does not.
        val rejection = ClipEndpoints.rejectionFor("http://100.101.102.103:8139/v1/clips")
        assertTrue(
            "the full endpoint was accepted as a base URL",
            rejection != null,
        )
        assertTrue(
            "the rejection does not say what is wrong, so the operator " +
                "retypes at random: [$rejection]",
            rejection!!.contains(ClipEndpoints.CLIPS_PATH),
        )
    }

    @Test
    fun whatCannotBeABaseUrlIsRejectedWithAReason() {
        // Each of these reaches nothing, and each would surface as a transport
        // failure — i.e. as "the mesh is down", which is the wrong diagnosis
        // forever. The over-long one is the 414 route: the body is never in
        // the URI, so an absurd base URL is the ONLY way to reach that status.
        for (bad in listOf(
            "100.101.102.103:8139",
            "ftp://100.101.102.103:8139",
            "http://",
            "http:///v1",
            "http://100.101.102.103:8139?limit=3",
            "http://100.101.102.103:8139#frag",
            "http://100.101.102 .103:8139",
            "http://" + "a".repeat(ClipEndpoints.MAX_BASE_URL_CHARS),
        )) {
            assertTrue(
                "[$bad] was accepted as a clip store base URL",
                ClipEndpoints.rejectionFor(bad) != null,
            )
        }
    }

    @Test
    fun theValidatorCannotCatchAVALIDUrlForTheWRONGService() {
        // 🔴 STATED AS A LIMIT, not discovered later. The capture-loss defect
        // was reached by pasting the TRANSCRIPTION base URL — `…:8118`, four
        // digits from `…:8139` — into the clip field, and that string is a
        // perfectly well-formed base URL. No syntactic check can tell two
        // valid mesh addresses apart.
        //
        // This case exists so nobody reads the validator as the fix. The fix
        // is that a 404 no longer destroys anything
        // (ClipHttp.PERMANENT_STATUSES); the validator only closes the cases
        // that are detectable.
        assertEquals(
            "the validator claims to catch a URL it cannot possibly catch",
            null,
            ClipEndpoints.rejectionFor("http://100.101.102.103:8118"),
        )
    }
}
