package online.phimnet.tv.playback

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException

@OptIn(ExperimentalCoroutinesApi::class)
class WatchSessionTest {

    private class Recorder {
        val beats = mutableListOf<String>()
        val leaves = mutableListOf<String>()
        var failBeats = false
    }

    private fun TestScope.session(rec: Recorder) = WatchSession(
        beat = { hash, _ -> if (rec.failBeats) throw IOException("offline"); rec.beats += hash },
        leave = { id -> rec.leaves += id },
        scope = backgroundScope,
        outlive = this,
        id = "session-1",
    )

    // The viewer drops a torrent after 30s without a heartbeat. The web page
    // beats every 10s, and so must this — immediately, then on the interval.
    @Test
    fun beatsImmediatelyThenEveryTenSeconds() = runTest {
        val rec = Recorder()
        session(rec).watch("hash-a")
        runCurrent()
        assertEquals(listOf("hash-a"), rec.beats)

        advanceTimeBy(10_001)
        assertEquals(2, rec.beats.size)
        advanceTimeBy(20_000)
        assertEquals(4, rec.beats.size)
    }

    // A quality switch is a different torrent. The new hash is sent at once so
    // the server can release the old torrent now, not up to ten seconds later.
    @Test
    fun switchingSourcesRepointsTheSessionImmediately() = runTest {
        val rec = Recorder()
        val s = session(rec)
        s.watch("hash-a")
        runCurrent()
        advanceTimeBy(3_000)
        s.watch("hash-b")
        runCurrent()
        assertEquals(listOf("hash-a", "hash-b"), rec.beats)

        advanceTimeBy(10_000)
        assertEquals("the loop keeps beating the NEW hash", "hash-b", rec.beats.last())
    }

    @Test
    fun endingTellsTheServerAndStopsBeating() = runTest {
        val rec = Recorder()
        val s = session(rec)
        s.watch("hash-a")
        runCurrent()
        s.end()
        runCurrent()
        assertEquals(listOf("session-1"), rec.leaves)

        val beatsAtEnd = rec.beats.size
        advanceTimeBy(60_000)
        assertEquals("no beats after end()", beatsAtEnd, rec.beats.size)
    }

    // One dropped request on a flaky link must never stop playback: the TTL is
    // three intervals, so the next beat still arrives in time.
    @Test
    fun aFailedBeatDoesNotStopTheLoop() = runTest {
        val rec = Recorder()
        rec.failBeats = true
        session(rec).watch("hash-a")
        runCurrent()
        rec.failBeats = false
        advanceTimeBy(10_001)
        assertEquals(listOf("hash-a"), rec.beats)
    }

    @Test
    fun endingWithoutEverWatchingSendsNothing() = runTest {
        val rec = Recorder()
        session(rec).end()
        runCurrent()
        assertTrue(rec.leaves.isEmpty())
    }

    // Backgrounding the app ends the session; coming back resumes it under the
    // same id, which the server simply re-creates.
    @Test
    fun aSessionCanResumeAfterEnding() = runTest {
        val rec = Recorder()
        val s = session(rec)
        s.watch("hash-a")
        runCurrent()
        s.end()
        runCurrent()
        s.watch("hash-a")
        runCurrent()
        assertEquals(listOf("hash-a", "hash-a"), rec.beats)
    }
}
