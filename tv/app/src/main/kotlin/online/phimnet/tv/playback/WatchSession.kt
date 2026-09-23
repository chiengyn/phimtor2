package online.phimnet.tv.playback

import android.util.Log
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.util.UUID
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

/**
 * Keeps the viewer told that this television is still watching, so the torrent
 * stays on its streamer — and tells it promptly when it stops, so it does not.
 *
 * This is NOT optional. The viewer's watchTracker reference-counts sessions per
 * torrent and drops a torrent once its last session has been silent for
 * WATCH_HEARTBEAT_TTL, which defaults to 30 SECONDS. A client that does not
 * heartbeat has its stream pulled out from under it half a minute into playback.
 * The web page beats every 10s; so does this.
 *
 * [watch] also re-points the session when the source changes (a quality switch
 * is a different torrent): the server's beat() sees the new infohash and drops
 * the old one if nobody else is on it.
 *
 * @param scope where the heartbeat loop runs; cancelled with the player.
 * @param outlive where the final leave runs. It must outlive [scope], because the
 *   leave is exactly the call that has to happen as the player is torn down.
 */
class WatchSession(
    private val beat: suspend (infoHash: String, sessionId: String) -> Unit,
    private val leave: suspend (sessionId: String) -> Unit,
    private val scope: CoroutineScope,
    private val outlive: CoroutineScope,
    private val interval: Duration = 10.seconds,
    val id: String = UUID.randomUUID().toString(),
) {
    @Volatile private var infoHash: String? = null
    private var loop: Job? = null

    /** Starts (or re-points) the heartbeat at [infoHash], beating immediately. */
    fun watch(infoHash: String) {
        val repointed = this.infoHash != null && this.infoHash != infoHash
        this.infoHash = infoHash
        if (loop?.isActive == true) {
            // Already beating: send the new hash now rather than up to 10s late,
            // so the old torrent is released promptly.
            if (repointed) scope.launch { beatOnce() }
            return
        }
        loop = scope.launch {
            while (isActive) {
                beatOnce()
                delay(interval)
            }
        }
    }

    /**
     * Ends the session: stops beating and tells the server now, so the torrent is
     * dropped at once if this was its last viewer rather than 30s later. Safe to
     * call twice, and the session can be resumed with [watch] afterwards.
     */
    fun end() {
        val wasWatching = infoHash != null
        loop?.cancel()
        loop = null
        infoHash = null
        if (wasWatching) {
            outlive.launch {
                try {
                    leave(id)
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    // Best effort, exactly like the web page's sendBeacon: if it is
                    // lost, the server's sweep reaps the session after the TTL.
                    logQuietly("leave failed: ${e.message}")
                }
            }
        }
    }

    private suspend fun beatOnce() {
        val hash = infoHash ?: return
        try {
            beat(hash, id)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // One missed beat is harmless — the TTL is three intervals — so a
            // network blip must never stop playback. Keep beating.
            logQuietly("heartbeat failed: ${e.message}")
        }
    }

    private fun logQuietly(message: String) {
        // android.util.Log is a stub under JVM unit tests; never let it throw here.
        runCatching { Log.w(TAG, message) }
    }

    private companion object {
        const val TAG = "WatchSession"
    }
}
