package online.phimnet.tv.playback

import androidx.media3.common.PlaybackException
import org.junit.Assert.assertEquals
import org.junit.Test

class RecoveryPolicyTest {

    // A 404 from the stream means the torrent was reaped. Preparing again is how
    // the web page's recoverStream heals it.
    @Test
    fun aReapedTorrentIsPreparedAgain() {
        assertEquals(
            Recovery.Reprepare,
            RecoveryPolicy.classify(PlaybackException.ERROR_CODE_IO_BAD_HTTP_STATUS, 404, false, 0),
        )
    }

    // A dead torrent must not loop forever.
    @Test
    fun rePreparingGivesUpAfterTheLimit() {
        assertEquals(
            Recovery.Fail,
            RecoveryPolicy.classify(
                PlaybackException.ERROR_CODE_IO_BAD_HTTP_STATUS, 404, false, RecoveryPolicy.MAX_REPREPARES,
            ),
        )
    }

    @Test
    fun aCodecThisDeviceCannotDecodeFallsBackToTheTranscode() {
        for (code in listOf(
            PlaybackException.ERROR_CODE_DECODER_INIT_FAILED,
            PlaybackException.ERROR_CODE_DECODING_FORMAT_UNSUPPORTED,
            PlaybackException.ERROR_CODE_AUDIO_TRACK_INIT_FAILED,
        )) {
            assertEquals("code $code", Recovery.Compatibility, RecoveryPolicy.classify(code, null, false, 0))
        }
    }

    // The fallback is used once. If the transcode fails too, stop.
    @Test
    fun theTranscodeIsOnlyTriedOnce() {
        assertEquals(
            Recovery.Fail,
            RecoveryPolicy.classify(PlaybackException.ERROR_CODE_DECODING_FAILED, null, true, 0),
        )
    }

    @Test
    fun aNetworkDropIsPreparedAgain() {
        assertEquals(
            Recovery.Reprepare,
            RecoveryPolicy.classify(PlaybackException.ERROR_CODE_IO_NETWORK_CONNECTION_FAILED, null, false, 1),
        )
    }

    @Test
    fun anythingElseFails() {
        assertEquals(
            Recovery.Fail,
            RecoveryPolicy.classify(PlaybackException.ERROR_CODE_DRM_UNSPECIFIED, null, false, 0),
        )
    }
}
