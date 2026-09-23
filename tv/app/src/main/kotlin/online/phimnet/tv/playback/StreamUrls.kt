package online.phimnet.tv.playback

import online.phimnet.tv.api.Prepared

/** How to ask the streamer for a file's bytes. */
enum class StreamMode {
    /**
     * The container's own bytes, served with real HTTP Range support. The
     * default, and the reason this app exists: ExoPlayer demuxes Matroska, AVI and
     * MPEG-TS natively, so nothing needs remuxing or re-encoding and every seek is
     * an ordinary range request the streamer can prioritise.
     */
    Direct,

    /**
     * The streamer's ffmpeg path: H.264 + stereo AAC in fragmented MP4. It
     * ignores Range, so it CANNOT seek, and it costs a CPU core on the streamer
     * per viewer. Only ever a fallback for a file whose codecs this device cannot
     * decode — the same last rung the web page's ladder falls to.
     */
    Compatibility,
}

/**
 * Builds streamer URLs exactly as the web watch page does
 * (`${streamerBase}/api/torrents/${infoHash}/files/${fileIndex}/stream`), so the
 * two clients can never disagree about the shape.
 */
object StreamUrls {

    /**
     * The stream URL for a prepared source.
     *
     * In [StreamMode.Direct] this ALWAYS carries `raw=1`. Without it the streamer
     * sees any container that is not mp4/webm/ogg — i.e. nearly every .mkv in the
     * catalog — and pipes it through ffmpeg, which looks like it works while
     * silently making the stream unseekable and burning server CPU. It is the
     * single most load-bearing detail of this client, so it is pinned by tests.
     */
    fun stream(prepared: Prepared, mode: StreamMode = StreamMode.Direct): String {
        val query = when (mode) {
            StreamMode.Direct -> "raw=1"
            StreamMode.Compatibility -> "transcode=1"
        }
        return "${base(prepared.streamerPublicURL)}/api/torrents/${prepared.infoHash}" +
            "/files/${prepared.fileIndex}/stream?$query"
    }

    fun stats(streamerBase: String, infoHash: String): String =
        "${base(streamerBase)}/api/torrents/$infoHash/stats"

    /**
     * The streamer's advertised public URL, minus any trailing slash. A path
     * prefix is preserved (a streamer may sit behind a reverse proxy at
     * /stream1), which is why this is string handling and not URL resolution —
     * resolving "/api/…" against it would throw the prefix away.
     */
    private fun base(url: String): String = url.trimEnd('/')
}
