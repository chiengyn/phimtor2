package online.phimnet.tv.ui.common

import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.focus.FocusDirection
import androidx.compose.ui.focus.FocusRequester

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
