package online.phimnet.tv.ui.common

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Border
import androidx.tv.material3.SelectableSurfaceDefaults
import androidx.tv.material3.Surface
import online.phimnet.tv.ui.theme.Accent
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted

/**
 * A selectable chip: genres, seasons, qualities, subtitle and audio tracks, the
 * language picker.
 *
 * Built on tv-material's stable selectable Surface rather than its FilterChip,
 * which 1.1 still marks experimental ("likely to change or be removed"). It is
 * also the one place chip styling lives: selected is the site's accent red,
 * focused is white, so which one is chosen and which one the remote is on are
 * never confused.
 */
@Composable
fun ChoiceChip(
    selected: Boolean,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    leading: (@Composable () -> Unit)? = null,
    content: @Composable () -> Unit,
) {
    val shape = RoundedCornerShape(20.dp)
    Surface(
        selected = selected,
        onClick = onClick,
        modifier = modifier,
        shape = SelectableSurfaceDefaults.shape(shape),
        colors = SelectableSurfaceDefaults.colors(
            containerColor = Surface2,
            contentColor = TextMuted,
            selectedContainerColor = Accent,
            selectedContentColor = Color.White,
            focusedContainerColor = Color.White,
            focusedContentColor = Color.Black,
            focusedSelectedContainerColor = Color.White,
            focusedSelectedContentColor = Accent,
        ),
        border = SelectableSurfaceDefaults.border(
            focusedSelectedBorder = Border(BorderStroke(2.dp, Accent), shape = shape),
        ),
        scale = SelectableSurfaceDefaults.scale(focusedScale = 1.05f),
    ) {
        Row(
            Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            leading?.invoke()
            content()
        }
    }
}
