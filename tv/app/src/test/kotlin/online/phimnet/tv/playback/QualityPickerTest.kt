package online.phimnet.tv.playback

import online.phimnet.tv.api.Video
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

// These cases mirror pickDefaultVideo in viewer/templates/watch.html. A person
// must get the same quality on their television as in their browser.
class QualityPickerTest {

    private fun video(id: Long, resolution: String, available: Boolean = true, lock: String = "") =
        Video(id = id, resolution = resolution, available = available, lock = if (available) "" else lock)

    @Test
    fun prefers1080pOver720p() {
        val picked = QualityPicker.pick(listOf(video(1, "720p"), video(2, "1080p")), remembered = null)
        assertEquals(2L, picked?.id)
    }

    // 4K is the heaviest source and the slowest to buffer from a swarm, so it is
    // opt-in even for someone entitled to it.
    @Test
    fun neverAutoPlays4kWhenSomethingSmallerIsPlayable() {
        val picked = QualityPicker.pick(
            listOf(video(1, "2160p"), video(2, "1080p"), video(3, "720p")),
            remembered = null,
        )
        assertEquals("1080p", picked?.resolution)
    }

    @Test
    fun plays4kWhenItIsTheOnlyPlayableSource() {
        val picked = QualityPicker.pick(listOf(video(1, "2160p")), remembered = null)
        assertEquals("2160p", picked?.resolution)
    }

    @Test
    fun honoursAQualityChosenByHand() {
        val picked = QualityPicker.pick(
            listOf(video(1, "2160p"), video(2, "1080p")),
            remembered = "2160p",
        )
        assertEquals("2160p", picked?.resolution)
    }

    // The remembered quality is a convenience, never an entitlement: after a pass
    // lapses, 4K is no longer playable and the preference must simply miss.
    @Test
    fun aRememberedQualityThatIsNoLongerPlayableFallsDownTheLadder() {
        val picked = QualityPicker.pick(
            listOf(video(1, "2160p", available = false, lock = "upgrade"), video(2, "1080p"), video(3, "720p")),
            remembered = "2160p",
        )
        assertEquals("1080p", picked?.resolution)
    }

    @Test
    fun anonymousVisitorLandsOn720pWhen1080pIsMembersOnly() {
        val picked = QualityPicker.pick(
            listOf(video(1, "1080p", available = false, lock = "member"), video(2, "720p")),
            remembered = null,
        )
        assertEquals("720p", picked?.resolution)
    }

    // The server returns sources newest first, and recency breaks ties within a
    // quality — so the first listed wins.
    @Test
    fun tiesWithinAQualityGoToTheFirstListed() {
        val picked = QualityPicker.pick(listOf(video(7, "1080p"), video(3, "1080p")), remembered = null)
        assertEquals(7L, picked?.id)
    }

    @Test
    fun nothingPlayableMeansNoPick() {
        assertNull(
            QualityPicker.pick(listOf(video(1, "1080p", available = false, lock = "member")), remembered = null),
        )
        assertNull(QualityPicker.pick(emptyList(), remembered = null))
    }

    // With nothing playable, sign-in is explained first: it is the cheapest thing
    // that might unlock something.
    @Test
    fun theGateExplainsSignInBeforeUpgradeBeforeComingSoon() {
        val locked = listOf(
            video(1, "2160p", available = false, lock = "paid"),
            video(2, "2160p", available = false, lock = "upgrade"),
            video(3, "1080p", available = false, lock = "member"),
        )
        assertEquals(3L, QualityPicker.gate(locked)?.id)
        assertEquals(2L, QualityPicker.gate(locked.take(2))?.id)
        assertEquals(1L, QualityPicker.gate(locked.take(1))?.id)
    }
}
