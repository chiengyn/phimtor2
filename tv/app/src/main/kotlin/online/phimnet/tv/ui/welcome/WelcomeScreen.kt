package online.phimnet.tv.ui.welcome

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.ui.common.ErrorBox
import online.phimnet.tv.ui.common.Load
import online.phimnet.tv.ui.common.LoadingBox
import online.phimnet.tv.ui.common.UpdatePanel
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.isActionable
import online.phimnet.tv.ui.common.rememberLoad
import online.phimnet.tv.ui.common.rememberSessionKey
import online.phimnet.tv.ui.link.LinkScreen

/**
 * Where an unpaired TV starts. The SERVER decides whether that matters: /me's
 * `sign_in_required` is true on a members-only viewer, and then this screen is
 * the pairing screen itself — code and QR up front, nothing to browse until it
 * is approved. Otherwise (a server without accounts, or one older than the
 * field) it steps aside to Home at once, as the app always behaved.
 *
 * [onOpen] leaves for Home; it runs on pairing too. Keyed on the session, so
 * coming back from Settings with another server asks that server afresh.
 */
@Composable
fun WelcomeScreen(onOpen: () -> Unit, onSettings: () -> Unit) {
    val graph = LocalGraph.current
    val me = rememberLoad(rememberSessionKey()) { graph.api.me() }

    when (val state = me.state) {
        Load.Loading -> LoadingBox()
        // Unreachable or misconfigured: Retry alone would trap someone whose
        // server address is simply wrong, so Settings is offered beside it.
        is Load.Failed -> ErrorBox(
            errorText(state.error),
            onRetry = me.retry,
            secondary = stringResource(R.string.action_settings) to onSettings,
        )
        is Load.Ready ->
            if (state.value.signedIn || !state.value.signInRequired) {
                LaunchedEffect(Unit) { onOpen() }
            } else {
                // An unpaired TV never reaches Home, where the update offer
                // normally waits, so it is shown here — a TV that cannot pair
                // because its app is too old must still be able to update.
                val update by graph.updater.state.collectAsStateWithLifecycle()
                Column(Modifier.fillMaxSize()) {
                    if (update.isActionable()) {
                        UpdatePanel(update, inSettings = false, modifier = Modifier.padding(top = 24.dp))
                    }
                    LinkScreen(onDone = onOpen, onSettings = onSettings)
                }
            }
    }
}
