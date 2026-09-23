package online.phimnet.tv.playback

import online.phimnet.tv.api.Lock
import online.phimnet.tv.api.Video
import online.phimnet.tv.api.lockKind

/**
 * Chooses which source starts playing. A straight port of the web watch page's
 * pickDefaultVideo (viewer/templates/watch.html) so a person gets the same
 * quality on their television as in their browser.
 *
 * The rules, and why:
 *  - Only sources the server marked playable are candidates. Which ones those
 *    are is resolutionLock's decision, made on the server; this never second-
 *    guesses it.
 *  - A quality the person picked by hand is honoured if it is still playable.
 *    It is a convenience, never an entitlement: a remembered "2160p" after a pass
 *    lapses simply misses the filter and falls down the ladder.
 *  - Otherwise walk [AUTO]: 1080p, then 720p. 2160p is deliberately NOT on it —
 *    it is the heaviest source and the slowest to buffer from a swarm, and
 *    auto-starting it gave paying users the worst first seconds of playback.
 *    4K is reached by choosing it, or when it is the only thing playable.
 *  - Ties within a quality go to the first listed, which is the newest: the
 *    server returns sources newest first.
 */
object QualityPicker {

    val AUTO: List<String> = listOf("1080p", "720p")

    fun pick(videos: List<Video>, remembered: String?): Video? {
        val playable = videos.filter { it.available }
        if (playable.isEmpty()) return null
        if (remembered != null) {
            playable.firstOrNull { it.resolution == remembered }?.let { return it }
        }
        for (resolution in AUTO) {
            playable.firstOrNull { it.resolution == resolution }?.let { return it }
        }
        return playable.first()
    }

    /**
     * When NOTHING is playable, the locked source worth explaining. Sign-in comes
     * first because it is the cheapest thing that might unlock something; an
     * upgrade next; "coming soon" last, since the person can do nothing about it.
     */
    fun gate(videos: List<Video>): Video? =
        listOf(Lock.Member, Lock.Upgrade, Lock.Paid).firstNotNullOfOrNull { lock ->
            videos.firstOrNull { !it.available && it.lockKind == lock }
        }
}
