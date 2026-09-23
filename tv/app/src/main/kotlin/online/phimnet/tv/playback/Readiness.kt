package online.phimnet.tv.playback

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.withTimeoutOrNull
import online.phimnet.tv.api.Stats
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

/**
 * Waits until the streamer knows the torrent's metadata, i.e. until a player can
 * open the stream and get a real byte range back. Mirrors the web page's
 * waitForStreamReady: poll every second, give up after a minute.
 *
 * The readiness signal is `totalBytes > 0` and NEVER `bytesCompleted > 0`. In
 * the streamer's download-all storage mode nothing downloads until a reader opens
 * the file, so waiting for downloaded bytes before opening the file would wait
 * forever. Metadata is all that is needed; the player's own read is what starts
 * the swarm.
 *
 * Returns false on timeout — in practice a torrent with no reachable seeders.
 */
suspend fun awaitStreamReady(
    poll: suspend () -> Stats?,
    onUpdate: (Stats?) -> Unit = {},
    timeout: Duration = 60.seconds,
    interval: Duration = 1.seconds,
): Boolean = withTimeoutOrNull(timeout) {
    var ready = false
    while (!ready) {
        val stats = try {
            poll()
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            null // a blip on the streamer is "not ready yet", not a failure
        }
        onUpdate(stats)
        ready = stats != null && stats.totalBytes > 0
        if (!ready) delay(interval)
    }
    true
} ?: false
