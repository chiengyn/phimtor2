package online.phimnet.tv.playback

import androidx.media3.common.PlaybackException

/** What to do after the player fails. */
enum class Recovery {
    /**
     * Ask the viewer to prepare the source again and resume where we were. A 404
     * from the stream means the torrent was reaped — by the streamer's idle
     * reaper, or by the viewer's watch-session sweep if heartbeats were lost —
     * and preparing again is exactly how the web page's recoverStream heals it.
     */
    Reprepare,

    /**
     * This device cannot decode the file (an HEVC 10-bit video on a weak SoC,
     * DTS audio with no decoder). Fall back once to the streamer's transcode —
     * the web page's last rung too. It cannot seek, so it is a last resort.
     */
    Compatibility,

    /** Nothing more to try; show the error. */
    Fail,
}

object RecoveryPolicy {

    /** Re-prepare attempts per source before giving up, so a dead torrent cannot loop forever. */
    const val MAX_REPREPARES = 3

    /**
     * Classifies a failure from its Media3 error code and, when the failure was an
     * HTTP answer, that answer's status. Kept free of Media3's exception types so
     * it can be tested on the JVM; the player unwraps them before calling this.
     */
    fun classify(
        errorCode: Int,
        httpStatus: Int?,
        inCompatibility: Boolean,
        reprepares: Int,
    ): Recovery {
        val canReprepare = reprepares < MAX_REPREPARES
        return when {
            httpStatus == 404 -> if (canReprepare) Recovery.Reprepare else Recovery.Fail
            errorCode in DECODE_FAILURES -> if (inCompatibility) Recovery.Fail else Recovery.Compatibility
            errorCode in NETWORK_FAILURES -> if (canReprepare) Recovery.Reprepare else Recovery.Fail
            else -> Recovery.Fail
        }
    }

    private val DECODE_FAILURES = setOf(
        PlaybackException.ERROR_CODE_DECODER_INIT_FAILED,
        PlaybackException.ERROR_CODE_DECODER_QUERY_FAILED,
        PlaybackException.ERROR_CODE_DECODING_FAILED,
        PlaybackException.ERROR_CODE_DECODING_FORMAT_EXCEEDS_CAPABILITIES,
        PlaybackException.ERROR_CODE_DECODING_FORMAT_UNSUPPORTED,
        PlaybackException.ERROR_CODE_PARSING_CONTAINER_UNSUPPORTED,
        PlaybackException.ERROR_CODE_AUDIO_TRACK_INIT_FAILED,
    )

    private val NETWORK_FAILURES = setOf(
        PlaybackException.ERROR_CODE_IO_UNSPECIFIED,
        PlaybackException.ERROR_CODE_IO_NETWORK_CONNECTION_FAILED,
        PlaybackException.ERROR_CODE_IO_NETWORK_CONNECTION_TIMEOUT,
        PlaybackException.ERROR_CODE_IO_BAD_HTTP_STATUS,
        PlaybackException.ERROR_CODE_TIMEOUT,
    )
}
