package online.phimnet.tv.settings

import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

/**
 * Validates the viewer address typed into Settings.
 *
 * phimtor2 is self-hosted, so the server is a setting and not a constant — and it
 * is typed with a remote, so it has to be forgiving: "phimnet.online" means
 * https://phimnet.online, and a trailing slash or stray whitespace is noise.
 */
object ServerUrl {

    sealed interface Result {
        data class Ok(val url: HttpUrl) : Result
        data object Invalid : Result
        /**
         * Plain http in a release build. The bearer token rides on every request,
         * so a release build refuses to send it in the clear. Debug builds allow
         * it, because a local `go run .` viewer speaks plain http.
         */
        data object CleartextNotAllowed : Result
    }

    fun parse(input: String, allowCleartext: Boolean): Result {
        val text = input.trim()
        if (text.isEmpty()) return Result.Invalid
        // Look for the scheme BEFORE trimming slashes: trimming first turns a bare
        // "https://" into "https:", which then reads as a hostname.
        val withScheme = if (SCHEME.containsMatchIn(text)) text else "https://$text"
        val url = withScheme.trimEnd('/').toHttpUrlOrNull() ?: return Result.Invalid
        if (url.host.isBlank() || url.query != null || url.fragment != null) return Result.Invalid
        if (!url.isHttps && !allowCleartext) return Result.CleartextNotAllowed
        return Result.Ok(url)
    }

    /** The canonical text form stored in settings: no trailing slash. */
    fun format(url: HttpUrl): String = url.toString().trimEnd('/')

    private val SCHEME = Regex("^[a-zA-Z][a-zA-Z0-9+.-]*://")
}
