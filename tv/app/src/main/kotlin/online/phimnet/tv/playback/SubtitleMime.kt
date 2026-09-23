package online.phimnet.tv.playback

import androidx.media3.common.MimeTypes
import online.phimnet.tv.api.Subtitle

/**
 * The MIME type a saved subtitle is served as.
 *
 * A browser hands the file to a <track> element and lets it work the format out;
 * ExoPlayer must be told before it opens the URL. The viewer serves "srt" rows as
 * application/x-subrip and everything else as text/vtt (subtitleContentType in
 * viewer/server.go), which is why the TV API grew a `format` field.
 */
fun Subtitle.mimeType(): String = when (format.lowercase()) {
    "srt" -> MimeTypes.APPLICATION_SUBRIP
    else -> MimeTypes.TEXT_VTT
}
