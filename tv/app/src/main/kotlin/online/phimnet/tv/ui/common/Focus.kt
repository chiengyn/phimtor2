package online.phimnet.tv.ui.common

import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Modifier
import androidx.compose.ui.composed
import androidx.compose.ui.focus.FocusDirection
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalFocusManager

/**
 * Moves focus to this target as soon as it can actually take it.
 *
 * A single requestFocus() from a LaunchedEffect can run in the very frame a panel
 * appears, before its button has been placed — and it then fails quietly. On a
 * phone that is cosmetic; on a television it leaves NOTHING focused, and the
 * remote has no way in until someone happens to press an arrow. So retry once
 * per frame, for a few frames, until the request reports success.
 */
suspend fun FocusRequester.focusWhenReady() {
    repeat(10) {
        withFrameNanos { }
        if (runCatching { requestFocus(FocusDirection.Enter) }.getOrDefault(false)) return
    }
}

/**
 * Lets ↑/↓ leave a single-line text field. BasicTextField takes them as "move the
 * cursor a line", so with the keyboard closed the remote could never get out of
 * the field and on to the controls below it (found on the emulator, under the
 * server field in Settings). ←/→ still move the cursor.
 */
fun Modifier.dpadLeavesTextField(): Modifier = composed {
    val focusManager = LocalFocusManager.current
    onPreviewKeyEvent { event ->
        val direction = when (event.key) {
            Key.DirectionDown -> FocusDirection.Down
            Key.DirectionUp -> FocusDirection.Up
            else -> return@onPreviewKeyEvent false
        }
        if (event.type == KeyEventType.KeyDown) focusManager.moveFocus(direction)
        true
    }
}
