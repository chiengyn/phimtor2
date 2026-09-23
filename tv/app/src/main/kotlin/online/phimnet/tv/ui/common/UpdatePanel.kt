package online.phimnet.tv.ui.common

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Button
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.OutlinedButton
import androidx.tv.material3.Text
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted
import online.phimnet.tv.update.UpdateState
import kotlin.math.roundToInt

/** States worth putting in front of someone on the Home screen. */
fun UpdateState.isActionable(): Boolean = when (this) {
    is UpdateState.Available, is UpdateState.Downloading, is UpdateState.Installing,
    is UpdateState.NeedsPermission -> true
    is UpdateState.Failed -> release != null
    else -> false
}

/**
 * The update offer and its progress. On Home it is a dismissible banner shown
 * only when there is something to do; in Settings it is always there, with a
 * manual check.
 */
@Composable
fun UpdatePanel(state: UpdateState, inSettings: Boolean, modifier: Modifier = Modifier) {
    val graph = LocalGraph.current
    val updater = graph.updater
    // The TV has no per-app "install unknown apps" screen (some builds hide it):
    // point at the Downloader route, which always works.
    var noSettingsScreen by remember { mutableStateOf(false) }

    // Every step replaces the panel's buttons, and a focused button that
    // disappears drops focus onto the top bar — where the next press of Down
    // lands on "Later" by accident. Once the person has pressed one of these
    // buttons, each later step that has buttons takes focus back to its first.
    val primary = remember { FocusRequester() }
    var followFocus by remember { mutableStateOf(false) }
    fun act(action: () -> Unit): () -> Unit = {
        followFocus = true
        action()
    }
    val first = Modifier.focusRequester(primary)
    val idle = state is UpdateState.Idle || state is UpdateState.UpToDate ||
        (state is UpdateState.Failed && state.release == null)
    val hasButtons = when (state) {
        is UpdateState.Available, is UpdateState.NeedsPermission -> true
        is UpdateState.Failed -> state.release != null || inSettings
        else -> inSettings && idle
    }
    LaunchedEffect(state::class, noSettingsScreen) {
        if (followFocus && hasButtons) primary.focusWhenReady()
    }

    val message: String? = when (state) {
        UpdateState.Idle, UpdateState.Checking -> if (inSettings) stringResource(R.string.update_checking) else null
        UpdateState.UpToDate -> if (inSettings) stringResource(R.string.update_up_to_date) else null
        is UpdateState.Available -> stringResource(R.string.update_available, state.release.versionName)
        is UpdateState.Downloading -> stringResource(R.string.update_downloading, (state.progress * 100).roundToInt())
        is UpdateState.Installing -> stringResource(R.string.update_installing)
        is UpdateState.NeedsPermission ->
            if (noSettingsScreen) stringResource(R.string.update_no_settings, graph.api.absolute("/tv"))
            else stringResource(R.string.update_permission)
        is UpdateState.Failed -> stringResource(
            when (state.reason) {
                UpdateState.Reason.CheckFailed -> R.string.update_check_failed
                UpdateState.Reason.Network -> R.string.error_network
                UpdateState.Reason.Corrupt -> R.string.update_corrupt
                UpdateState.Reason.InstallFailed -> R.string.update_install_failed
            },
        )
    }

    val boxed = if (inSettings) modifier else modifier
        .padding(horizontal = 48.dp)
        .fillMaxWidth()
        .background(Surface2, RoundedCornerShape(8.dp))
        .padding(horizontal = 24.dp, vertical = 16.dp)

    Column(boxed, verticalArrangement = Arrangement.spacedBy(12.dp)) {
        message?.let {
            Text(
                it,
                style = if (inSettings) MaterialTheme.typography.bodyLarge else MaterialTheme.typography.titleMedium,
                color = if (inSettings) TextMuted else MaterialTheme.colorScheme.onSurface,
            )
        }
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
            when (state) {
                is UpdateState.Available -> {
                    Button(onClick = act(updater::install), modifier = first) { Text(stringResource(R.string.update_now)) }
                    if (!inSettings) {
                        OutlinedButton(onClick = act(updater::dismiss)) { Text(stringResource(R.string.update_later)) }
                    }
                }
                is UpdateState.NeedsPermission -> {
                    if (!noSettingsScreen) {
                        Button(
                            onClick = act { noSettingsScreen = !updater.openInstallPermissionSettings() },
                            modifier = first,
                        ) {
                            Text(stringResource(R.string.update_open_settings))
                        }
                    }
                    OutlinedButton(
                        onClick = act(updater::install),
                        modifier = if (noSettingsScreen) first else Modifier,
                    ) { Text(stringResource(R.string.update_continue)) }
                }
                is UpdateState.Failed -> if (state.release != null) {
                    Button(onClick = act(updater::install), modifier = first) { Text(stringResource(R.string.action_retry)) }
                    if (!inSettings) {
                        OutlinedButton(onClick = act(updater::dismiss)) { Text(stringResource(R.string.update_later)) }
                    }
                }
                else -> Unit
            }
            if (inSettings && idle) {
                OutlinedButton(onClick = act { updater.check(manual = true) }, modifier = first) {
                    Text(stringResource(R.string.update_check))
                }
            }
        }
    }
}
