package online.phimnet.tv.ui.common

import android.graphics.Bitmap
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.FilterQuality
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Border
import androidx.tv.material3.Button
import androidx.tv.material3.Card
import androidx.tv.material3.CardDefaults
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Text
import coil3.compose.AsyncImage
import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import online.phimnet.tv.R
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.theme.Accent
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted
import android.graphics.Color as AndroidColor
import online.phimnet.tv.api.Card as TitleCard

/** Poster size for rows and grids. Tuned for a 1080p TV at couch distance. */
val PosterWidth = 150.dp
val PosterHeight = 225.dp

@Composable
fun LoadingBox(modifier: Modifier = Modifier, label: String? = null) {
    Box(modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Text(
            label ?: stringResource(R.string.state_loading),
            style = MaterialTheme.typography.titleMedium,
            color = TextMuted,
        )
    }
}

/**
 * An error with a way out. The retry button takes focus at once: on a TV a
 * failure screen without a focused action is a dead end the remote cannot leave.
 */
@Composable
fun ErrorBox(message: String, onRetry: () -> Unit, modifier: Modifier = Modifier) {
    val focus = remember { FocusRequester() }
    Column(
        modifier.fillMaxSize().padding(48.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(message, style = MaterialTheme.typography.titleMedium, textAlign = TextAlign.Center)
        Spacer(Modifier.height(24.dp))
        Button(onClick = onRetry, modifier = Modifier.focusRequester(focus)) {
            Text(stringResource(R.string.action_retry))
        }
    }
    LaunchedEffect(Unit) { focus.focusWhenReady() }
}

/** A title's poster. [rank] draws the "Top 10" numeral the website's ranked row has. */
@Composable
fun PosterCard(
    card: TitleCard,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    rank: Int? = null,
) {
    Column(modifier.width(PosterWidth)) {
        Card(
            onClick = onClick,
            modifier = Modifier.width(PosterWidth).height(PosterHeight),
            shape = CardDefaults.shape(RoundedCornerShape(6.dp)),
            scale = CardDefaults.scale(focusedScale = 1.08f),
            border = CardDefaults.border(
                focusedBorder = Border(
                    border = androidx.compose.foundation.BorderStroke(3.dp, Color.White),
                    shape = RoundedCornerShape(6.dp),
                ),
            ),
        ) {
            Box(Modifier.fillMaxSize().background(Surface2)) {
                // The title is drawn UNDER the poster rather than instead of it,
                // so a poster that fails to load (a CDN hiccup, a stale TMDB path)
                // leaves a readable card instead of a blank grey box.
                Text(
                    card.title,
                    modifier = Modifier.align(Alignment.Center).padding(12.dp),
                    textAlign = TextAlign.Center,
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (card.poster.isNotEmpty()) {
                    AsyncImage(
                        model = card.poster,
                        contentDescription = card.title,
                        contentScale = ContentScale.Crop,
                        modifier = Modifier.fillMaxSize(),
                    )
                }
                if (rank != null) {
                    Text(
                        rank.toString(),
                        modifier = Modifier
                            .align(Alignment.BottomStart)
                            .background(Brush.verticalGradient(listOf(Color.Transparent, Color(0xCC000000))))
                            .padding(horizontal = 10.dp, vertical = 2.dp),
                        style = MaterialTheme.typography.displaySmall.copy(fontWeight = FontWeight.Black),
                    )
                }
                if (card.hasSubtitle) Badge(stringResource(R.string.badge_subtitle), Modifier.align(Alignment.TopEnd))
            }
        }
        // Room for the focused card's 1.08× scale (about 9dp at the bottom edge),
        // so a focused poster never draws over its own title.
        Spacer(Modifier.height(16.dp))
        Text(
            card.title,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            style = MaterialTheme.typography.bodySmall,
        )
        if (card.year.isNotEmpty()) {
            Text(card.year, style = MaterialTheme.typography.labelSmall, color = TextMuted)
        }
    }
}

@Composable
fun Badge(text: String, modifier: Modifier = Modifier) {
    Text(
        text,
        modifier = modifier
            .padding(6.dp)
            .clip(RoundedCornerShape(3.dp))
            .background(Accent)
            .padding(horizontal = 6.dp, vertical = 2.dp),
        style = MaterialTheme.typography.labelSmall.copy(fontSize = 11.sp, fontWeight = FontWeight.Bold),
        color = Color.White,
    )
}

/** A row of text chips separated by a middle dot: "2010 · 148 min · ★ 8.4". */
@Composable
fun MetaLine(parts: List<String>, modifier: Modifier = Modifier) {
    Row(modifier, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        parts.filter { it.isNotBlank() }.forEachIndexed { i, part ->
            if (i > 0) Text("·", color = TextMuted)
            Text(part, color = TextMuted, style = MaterialTheme.typography.bodyMedium)
        }
    }
}

/**
 * A QR code for a URL a person should open on their phone — the pairing link and
 * the upgrade link. Nobody types a URL with a remote, and nobody pays with one.
 */
@Composable
fun QrCode(content: String, size: Dp, modifier: Modifier = Modifier) {
    val bitmap = remember(content) { qrBitmap(content) }
    Box(
        modifier
            .size(size)
            .clip(RoundedCornerShape(8.dp))
            .background(Color.White)
            .padding(10.dp),
    ) {
        Image(
            bitmap = bitmap,
            contentDescription = content,
            modifier = Modifier.fillMaxSize(),
            // Scale the modules up without smoothing, so every edge stays crisp.
            filterQuality = FilterQuality.None,
        )
    }
}

private fun qrBitmap(content: String): ImageBitmap {
    val matrix = QRCodeWriter().encode(
        content,
        BarcodeFormat.QR_CODE,
        0,
        0,
        mapOf(EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.M, EncodeHintType.MARGIN to 0),
    )
    val pixels = IntArray(matrix.width * matrix.height) { i ->
        if (matrix.get(i % matrix.width, i / matrix.width)) AndroidColor.BLACK else AndroidColor.WHITE
    }
    return Bitmap.createBitmap(pixels, matrix.width, matrix.height, Bitmap.Config.ARGB_8888).asImageBitmap()
}

/** Section heading used above rows and settings groups. */
@Composable
fun SectionTitle(text: String, modifier: Modifier = Modifier) {
    Text(
        text,
        modifier = modifier.fillMaxWidth(),
        style = MaterialTheme.typography.titleLarge,
    )
}
