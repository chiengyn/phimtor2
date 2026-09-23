package online.phimnet.tv.update

import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import online.phimnet.tv.api.AppRelease

/**
 * Decides whether a release the viewer reports is one this app should offer.
 *
 * Android itself refuses an update signed by a different key, which is the real
 * guarantee that nobody can push a foreign app onto a TV this way. These checks
 * sit in front of that: never offer to fetch a file from anywhere but the
 * configured viewer, and never start a download the manifest cannot describe.
 */
object UpdatePolicy {

    /** Far above the real APK (~3 MB); a manifest claiming more is wrong, not big. */
    const val MAX_APK_BYTES: Long = 200L shl 20

    private val SHA256_HEX = Regex("^[0-9a-f]{64}$")

    fun offer(release: AppRelease?, installedVersionCode: Long, server: HttpUrl): AppRelease? {
        if (release == null) return null
        // Strictly newer: Android refuses an equal or lower versionCode anyway
        // (INSTALL_FAILED_VERSION_DOWNGRADE), so offering one would only fail.
        if (release.versionCode <= installedVersionCode) return null
        if (!sameOrigin(release.downloadUrl, server)) return null
        if (release.size !in 1..MAX_APK_BYTES) return null
        if (!SHA256_HEX.matches(release.sha256)) return null
        return release
    }

    /**
     * The download must come from the viewer this TV is configured for: same
     * scheme, host and port. The manifest is served by that viewer, so a URL
     * pointing anywhere else means something is wrong.
     */
    fun sameOrigin(url: String, server: HttpUrl): Boolean {
        val u = url.toHttpUrlOrNull() ?: return false
        return u.scheme == server.scheme && u.host == server.host && u.port == server.port
    }
}
