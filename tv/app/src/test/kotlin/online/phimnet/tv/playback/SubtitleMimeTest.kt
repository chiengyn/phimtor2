package online.phimnet.tv.playback

import androidx.media3.common.MimeTypes
import online.phimnet.tv.api.Subtitle
import org.junit.Assert.assertEquals
import org.junit.Test

// The viewer serves "srt" rows as application/x-subrip and everything else as
// text/vtt. ExoPlayer has to be told which before it opens the file.
class SubtitleMimeTest {
    @Test
    fun srtRowsAreSubRip() {
        assertEquals(MimeTypes.APPLICATION_SUBRIP, Subtitle(id = 1, format = "srt").mimeType())
        assertEquals(MimeTypes.APPLICATION_SUBRIP, Subtitle(id = 1, format = "SRT").mimeType())
    }

    @Test
    fun everythingElseIsWebVtt() {
        assertEquals(MimeTypes.TEXT_VTT, Subtitle(id = 1, format = "vtt").mimeType())
        assertEquals(MimeTypes.TEXT_VTT, Subtitle(id = 1, format = "").mimeType())
    }
}
