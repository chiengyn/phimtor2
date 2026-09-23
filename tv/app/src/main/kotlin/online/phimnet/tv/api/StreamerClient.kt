package online.phimnet.tv.api

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import online.phimnet.tv.playback.StreamUrls

/**
 * Reads a streamer's public stats endpoint — the only streamer route besides the
 * stream itself that a client may call.
 *
 * No credential is sent: the streamer is another host, and the viewer's bearer
 * token must never leave the viewer (see PhimnetApi).
 */
class StreamerClient(private val http: OkHttpClient, private val userAgent: String) {

    /**
     * Returns null while the torrent is not yet known to the streamer (a 404, which
     * is normal for the first moments after prepare, and again after the idle
     * reaper drops it). Any other failure is thrown, and the readiness loop treats
     * it as "not ready yet" too.
     */
    suspend fun stats(streamerBase: String, infoHash: String): Stats? {
        val request = Request.Builder()
            .url(StreamUrls.stats(streamerBase, infoHash))
            .header("User-Agent", userAgent)
            .build()
        // On IO for the same reason as PhimnetApi.execute: the body read is network I/O.
        return withContext(Dispatchers.IO) {
            http.newCall(request).await().use { response ->
                when {
                    response.code == 404 -> null
                    !response.isSuccessful -> throw response.toApiException()
                    else -> WireJson.decodeFromString(Stats.serializer(), response.body.string())
                }
            }
        }
    }
}
