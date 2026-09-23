package online.phimnet.tv.playback

import online.phimnet.tv.api.Prepared
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StreamUrlsTest {

    private val prepared = Prepared(
        infoHash = "c9e15763f722f23e98a29decdfae341b98d53056",
        fileIndex = 2,
        streamerPublicURL = "https://stream.example",
    )

    // The single most load-bearing detail of this client. Without raw=1 the
    // streamer transcodes every .mkv through ffmpeg: it plays, but it can never
    // seek, and it costs a CPU core per viewer. Nothing else would notice.
    @Test
    fun directStreamAlwaysAsksForRawBytes() {
        val url = StreamUrls.stream(prepared)
        assertEquals(
            "https://stream.example/api/torrents/c9e15763f722f23e98a29decdfae341b98d53056/files/2/stream?raw=1",
            url,
        )
        assertFalse("direct play must never request a transcode", "transcode" in url)
    }

    @Test
    fun compatibilityModeAsksForTheTranscodeAndNotRaw() {
        val url = StreamUrls.stream(prepared, StreamMode.Compatibility)
        assertTrue(url.endsWith("/files/2/stream?transcode=1"))
        // The streamer lets raw=1 win over transcode=1, so sending both would
        // silently disable the fallback.
        assertFalse("raw" in url)
    }

    @Test
    fun trailingSlashOnTheStreamerUrlIsIgnored() {
        val url = StreamUrls.stream(prepared.copy(streamerPublicURL = "https://stream.example/"))
        assertTrue(url.startsWith("https://stream.example/api/torrents/"))
    }

    // A streamer can sit behind a reverse proxy under a path. Resolving "/api/…"
    // against its URL as a URL would throw that prefix away; the web page keeps
    // it by concatenating, and so must this.
    @Test
    fun aPathPrefixOnTheStreamerIsPreserved() {
        val url = StreamUrls.stream(prepared.copy(streamerPublicURL = "https://proxy.example/stream1/"))
        assertTrue(url, url.startsWith("https://proxy.example/stream1/api/torrents/"))
        assertEquals(
            "https://proxy.example/stream1/api/torrents/abc/stats",
            StreamUrls.stats("https://proxy.example/stream1", "abc"),
        )
    }
}
