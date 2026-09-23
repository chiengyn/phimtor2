package online.phimnet.tv.playback

import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runTest
import online.phimnet.tv.api.Stats
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException

class ReadinessTest {

    // The deadlock this guards against: in the streamer's download-all mode
    // nothing downloads until a reader opens the file. Metadata (totalBytes) is
    // the signal; waiting for downloaded bytes would wait forever.
    @Test
    fun readyAsSoonAsMetadataIsKnownEvenWithNothingDownloaded() = runTest {
        val ready = awaitStreamReady(poll = { Stats(totalBytes = 1_000_000, bytesCompleted = 0) })
        assertTrue(ready)
        assertEquals("should not have waited at all", 0L, currentTime)
    }

    @Test
    fun keepsPollingThroughTheInitial404s() = runTest {
        var calls = 0
        val ready = awaitStreamReady(poll = {
            calls++
            if (calls < 4) null else Stats(totalBytes = 10)
        })
        assertTrue(ready)
        assertEquals(4, calls)
        assertEquals("one second between polls", 3_000L, currentTime)
    }

    @Test
    fun aFailingStreamerIsNotReadyYetRatherThanFatal() = runTest {
        var calls = 0
        val ready = awaitStreamReady(poll = {
            calls++
            if (calls == 1) throw IOException("connection reset") else Stats(totalBytes = 10)
        })
        assertTrue(ready)
    }

    @Test
    fun givesUpAfterAMinuteWithoutMetadata() = runTest {
        val ready = awaitStreamReady(poll = { Stats(totalBytes = 0) })
        assertFalse(ready)
        assertEquals(60_000L, currentTime)
    }

    @Test
    fun reportsEachReadingForTheLoadingScreen() = runTest {
        val seen = mutableListOf<Stats?>()
        var calls = 0
        awaitStreamReady(
            poll = { calls++; if (calls < 3) Stats(activePeers = calls) else Stats(totalBytes = 1) },
            onUpdate = { seen += it },
        )
        assertEquals(listOf(1, 2, 0), seen.map { it?.activePeers })
    }
}
