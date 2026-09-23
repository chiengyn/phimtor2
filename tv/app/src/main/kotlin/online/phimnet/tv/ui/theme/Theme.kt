package online.phimnet.tv.ui.theme

import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.darkColorScheme

// The website's own tokens (viewer/static/style.css), so the television feels
// like the same service: --bg #141414, the three surfaces, --accent #e50914.
val Background = Color(0xFF141414)
val Surface1 = Color(0xFF1F1F1F)
val Surface2 = Color(0xFF2A2A2A)
val Surface3 = Color(0xFF363636)
val TextPrimary = Color(0xFFFFFFFF)
val TextMuted = Color(0xFFB3B3B3)
val TextFaint = Color(0xFF808080)
val Accent = Color(0xFFE50914)

private val Scheme = darkColorScheme(
    primary = Accent,
    onPrimary = TextPrimary,
    secondary = Surface3,
    onSecondary = TextPrimary,
    background = Background,
    onBackground = TextPrimary,
    surface = Surface1,
    onSurface = TextPrimary,
    surfaceVariant = Surface2,
    onSurfaceVariant = TextMuted,
    border = Surface3,
    inverseSurface = TextPrimary,
    inverseOnSurface = Background,
)

@Composable
fun PhimnetTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = Scheme, content = content)
}
