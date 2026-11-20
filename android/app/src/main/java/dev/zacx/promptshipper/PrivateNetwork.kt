package dev.zacx.promptshipper

import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/**
 * The one rule for where this app may speak **plain HTTP**: only to an IP
 * literal in a private range. Everything else must be `https://`.
 *
 * ## Why the app enforces it, not `network_security_config.xml`
 *
 * The servers (`server/stt`, `server/clip`) have no TLS of their own and are
 * reached over a private network — a Tailscale tailnet, a home LAN, a VPN — so
 * the pairing payload carries `http://<private ip>:<port>` (see
 * `docs/pairing.md`, *URL rules*). Android's network security config can only
 * name **domains**, exactly or with subdomains; it has no notion of an address
 * RANGE, and the address is not known until the user pairs. So the platform
 * permits cleartext (`res/xml/network_security_config.xml`) and this object is
 * the gate, applied at both places a URL can enter or leave:
 *
 *  * **on the way in** — [rejectionFor] refuses to store a base URL that would
 *    need cleartext to a public host (pairing, Settings, the debug door);
 *  * **on the way out** — [open] is the production connection factory of both
 *    HTTP clients ([SttHttp], [ClipHttp]) and refuses the same URL again, so a
 *    value that reached the preferences some other way still never leaves the
 *    phone in cleartext. It also turns off redirect following: a private host
 *    answering `302 Location: http://<public host>/` would otherwise be
 *    followed in cleartext, carrying the bearer token.
 *
 * ## What counts as private
 *
 * Only **IP literals**, parsed here without DNS:
 *
 *  * IPv4 `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` (RFC 1918),
 *    `100.64.0.0/10` (CGNAT — the range Tailscale assigns), `127.0.0.0/8`;
 *  * IPv6 `::1` and `fd00::/8` (unique-local; Tailscale's `fd7a:115c:a1e0::/48`
 *    is inside it).
 *
 * `localhost` is deliberately NOT an exception (audit D2): it is a name the
 * platform resolves like any other, and the loopback literals cover the case.
 *
 * 🔴 **A hostname is never private**, whatever it resolves to — including
 * Tailscale MagicDNS names (`*.ts.net`) and LAN names (`*.local`, `*.lan`). The
 * name is resolved by the platform *after* this check, so a check on the name
 * cannot know where the bytes go, and a DNS answer can change between the
 * check and the connect. A tailnet name gets a real certificate from
 * `tailscale serve`/`tailscale cert`, so `https://<host>.ts.net` is the way to
 * use one; `./setup` emits the tailnet IP, which needs neither.
 */
object PrivateNetwork {

    /**
     * Why [url] may not be used, or null when it may. Only the scheme and the
     * host are judged here — the URL's shape (no path, a port) is
     * [BaseUrlRule]'s job.
     */
    fun cleartextRejection(url: String): String? {
        val trimmed = url.trim()
        val scheme = trimmed.substringBefore("://", missingDelimiterValue = "").lowercase()
        if (scheme == "https") return null
        if (scheme != "http") return "it must start with http:// or https://"
        val host = hostOf(trimmed) ?: return "it has no host"
        if (isPrivateHost(host)) return null
        return "plain http:// is only allowed to a private IP address " +
            "(10.x, 172.16–31.x, 192.168.x, 100.64–127.x, 127.x, fd00::/8) — " +
            "use https:// for $host"
    }

    /**
     * The production connection factory for both HTTP clients.
     *
     * @throws IOException when [cleartextRejection] refuses [url]. An
     *   `IOException` because both clients already map it to their
     *   "could not reach the server" outcome, with the message as the reason.
     */
    fun open(url: String): HttpURLConnection {
        cleartextRejection(url)?.let { throw IOException("refused: $it") }
        return (URL(url).openConnection() as HttpURLConnection).apply {
            instanceFollowRedirects = false
        }
    }

    /**
     * The host part of `scheme://[userinfo@]host[:port][/...]`, brackets
     * removed from an IPv6 literal, or null when there is none.
     */
    internal fun hostOf(url: String): String? {
        val authority = url.substringAfter("://", missingDelimiterValue = "")
            .substringBefore('/').substringBefore('?').substringBefore('#')
            .substringAfterLast('@')
        if (authority.isEmpty()) return null
        val host = if (authority.startsWith("[")) {
            val close = authority.indexOf(']')
            if (close < 0) return null
            authority.substring(1, close)
        } else {
            authority.substringBefore(':')
        }
        return host.takeIf { it.isNotEmpty() }
    }

    fun isPrivateHost(host: String): Boolean {
        val h = host.lowercase()
        ipv4(h)?.let { return isPrivateV4(it) }
        ipv6(h)?.let { return isPrivateV6(it) }
        return false
    }

    private fun isPrivateV4(a: IntArray): Boolean = when {
        a[0] == 10 -> true
        a[0] == 172 && a[1] in 16..31 -> true
        a[0] == 192 && a[1] == 168 -> true
        a[0] == 100 && a[1] in 64..127 -> true
        a[0] == 127 -> true
        else -> false
    }

    private fun isPrivateV6(h: IntArray): Boolean {
        val loopback = h.take(7).all { it == 0 } && h[7] == 1
        val uniqueLocal = (h[0] ushr 8) == 0xfd
        return loopback || uniqueLocal
    }

    /**
     * Strict dotted-quad: exactly four decimal octets, 0..255, no leading zeros
     * (`010.0.0.1` is octal to some parsers and decimal to others, so it is
     * refused rather than guessed at).
     */
    internal fun ipv4(s: String): IntArray? {
        val parts = s.split('.')
        if (parts.size != 4) return null
        val out = IntArray(4)
        for ((i, p) in parts.withIndex()) {
            if (p.isEmpty() || p.length > 3 || !p.all { it in '0'..'9' }) return null
            if (p.length > 1 && p[0] == '0') return null
            val v = p.toInt()
            if (v > 255) return null
            out[i] = v
        }
        return out
    }

    /**
     * An IPv6 literal as eight 16-bit groups, or null. Accepts `::`
     * compression; refuses a zone id (`%wlan0`) and an embedded IPv4 tail,
     * neither of which a pairing payload contains.
     */
    internal fun ipv6(s: String): IntArray? {
        if (!s.contains(':') || s.contains('%') || s.contains('.')) return null
        val halves = s.split("::")
        if (halves.size > 2) return null
        fun groups(part: String): List<Int>? {
            if (part.isEmpty()) return emptyList()
            return part.split(':').map { g ->
                if (g.isEmpty() || g.length > 4 || !g.all { it in '0'..'9' || it in 'a'..'f' }) {
                    return null
                }
                g.toInt(16)
            }
        }
        val head = groups(halves[0]) ?: return null
        val tail = if (halves.size == 2) groups(halves[1]) ?: return null else emptyList()
        val total = head.size + tail.size
        val all = if (halves.size == 2) {
            if (total > 7) return null
            head + List(8 - total) { 0 } + tail
        } else {
            if (total != 8) return null
            head
        }
        return all.toIntArray()
    }
}

/**
 * The shape rule for a server BASE url — shared by the transcription and clip
 * settings, the pairing payload and the debug door, so they cannot disagree.
 *
 * Moved here from `ClipEndpoints.rejectionFor` (whose kdoc still explains each
 * rejection) when the transcription URL gained the same check, and extended
 * with [PrivateNetwork.cleartextRejection].
 */
object BaseUrlRule {

    /** See `ClipEndpoints.MAX_BASE_URL_CHARS`. */
    const val MAX_BASE_URL_CHARS: Int = 2_048

    /**
     * @param servicePath the path the client appends, named in the message
     *   when the user pasted a full endpoint instead of a base URL.
     * @return a short reason, or null when [baseUrl] is acceptable. An EMPTY
     *   string is acceptable here — it means "clear the setting".
     */
    fun rejectionFor(baseUrl: String, servicePath: String): String? {
        val trimmed = baseUrl.trim()
        if (trimmed.isEmpty()) return null
        if (trimmed.length > MAX_BASE_URL_CHARS) {
            return "it is longer than $MAX_BASE_URL_CHARS characters"
        }
        val scheme = trimmed.substringBefore("://", missingDelimiterValue = "")
        if (scheme != "http" && scheme != "https") {
            return "it must start with http:// or https://"
        }
        val authority = trimmed.substringAfter("://")
        if (authority.isEmpty()) return "it has no host"
        val afterHost = authority.trimEnd('/')
        if (afterHost.isEmpty()) return "it has no host"
        if (afterHost.any { it == '/' || it == '?' || it == '#' }) {
            return "it must be a host and port only, with no path — " +
                "promptshipper appends $servicePath itself"
        }
        if (afterHost.any { it.isWhitespace() }) return "it contains a space"
        if (afterHost.contains('@')) return "it must not contain a user name or password"
        return PrivateNetwork.cleartextRejection(trimmed)
    }
}
