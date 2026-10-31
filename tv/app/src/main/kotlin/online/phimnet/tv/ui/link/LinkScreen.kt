package online.phimnet.tv.ui.link

import android.content.Context
import android.os.Build
import android.provider.Settings
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.OutlinedButton
import androidx.tv.material3.Text
import kotlinx.coroutines.CancellationException
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.auth.PairingState
import online.phimnet.tv.auth.pairingFlow
import online.phimnet.tv.ui.common.ErrorBox
import online.phimnet.tv.ui.common.LoadingBox
import online.phimnet.tv.ui.common.QrCode
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.theme.TextMuted

/**
 * Sign-in for a television: show a code, let a phone that is already signed in
 * approve it. No keyboard, no password, and no Google token ever on the TV.
 *
 * Reached two ways. From a gate or Settings it is "Sign in", with Back. As the
 * FRONT DOOR of a members-only server ([onSettings] set, see WelcomeScreen) it
 * is the first thing an unpaired TV shows: there is no Back to go to, so the
 * way out is Settings instead — where a wrong server address is fixed.
 */
@Composable
fun LinkScreen(onDone: () -> Unit, onSettings: (() -> Unit)? = null) {
    val graph = LocalGraph.current
    val context = LocalContext.current
    var state by remember { mutableStateOf<PairingState>(PairingState.Requesting) }
    var error by remember { mutableStateOf<Throwable?>(null) }
    var attempt by remember { mutableIntStateOf(0) }

    LaunchedEffect(attempt) {
        error = null
        try {
            pairingFlow(graph.api, deviceName(context)).collect { next ->
                state = next
                if (next is PairingState.Paired) {
                    val user = next.token.user
                    graph.settings.signIn(next.token.accessToken, user.name.ifBlank { user.email })
                    onDone()
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            error = e
        }
    }

    val failure = error
    val settings = onSettings?.let { stringResource(R.string.action_settings) to it }
    when {
        failure != null -> ErrorBox(errorText(failure), onRetry = { attempt++ }, secondary = settings)
        state is PairingState.Showing -> CodePanel(state as PairingState.Showing, onDone, onSettings)
        // Left unattended for an hour: stop polling until someone asks again.
        state is PairingState.Idle ->
            ErrorBox(stringResource(R.string.link_idle), onRetry = { attempt++ }, secondary = settings)
        else -> LoadingBox()
    }
}

@Composable
private fun CodePanel(showing: PairingState.Showing, onBack: () -> Unit, onSettings: (() -> Unit)?) {
    val code = showing.code
    val back = remember { FocusRequester() }
    val frontDoor = onSettings != null
    Row(
        Modifier.fillMaxSize().padding(horizontal = 96.dp, vertical = 64.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(72.dp),
    ) {
        Column(Modifier.weight(1f)) {
            if (frontDoor) {
                Text(stringResource(R.string.welcome_heading), style = MaterialTheme.typography.displaySmall)
                Spacer(Modifier.height(12.dp))
                Text(stringResource(R.string.welcome_intro), color = TextMuted, style = MaterialTheme.typography.titleMedium)
            } else {
                Text(stringResource(R.string.action_sign_in), style = MaterialTheme.typography.displaySmall)
            }
            Spacer(Modifier.height(32.dp))
            Text(stringResource(R.string.link_step_open), color = TextMuted, style = MaterialTheme.typography.titleMedium)
            Text(code.verificationUri, style = MaterialTheme.typography.headlineSmall)
            Spacer(Modifier.height(20.dp))
            Text(stringResource(R.string.link_step_code), color = TextMuted, style = MaterialTheme.typography.titleMedium)
            // Monospaced and spaced out: it is read from across a room.
            Text(
                code.userCode,
                style = MaterialTheme.typography.displayMedium.copy(
                    fontFamily = FontFamily.Monospace,
                    fontWeight = FontWeight.Bold,
                    letterSpacing = 6.sp,
                ),
            )
            Spacer(Modifier.height(24.dp))
            Text(stringResource(R.string.link_waiting), style = MaterialTheme.typography.bodyLarge)
            Text(stringResource(R.string.link_refresh_note), color = TextMuted, style = MaterialTheme.typography.bodyMedium)
            Spacer(Modifier.height(28.dp))
            OutlinedButton(onClick = onSettings ?: onBack, modifier = Modifier.focusRequester(back)) {
                Text(stringResource(if (frontDoor) R.string.action_settings else R.string.action_back))
            }
        }
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            // The "complete" link carries the code, so the phone lands on a
            // pre-filled page and the person only has to press Connect.
            QrCode(code.verificationUriComplete.ifBlank { code.verificationUri }, 280.dp)
            Spacer(Modifier.height(16.dp))
            Text(stringResource(R.string.link_scan), color = TextMuted, textAlign = TextAlign.Center)
        }
    }
    LaunchedEffect(Unit) { back.focusWhenReady() }
}

/**
 * What this television is called, for the account's list of paired devices.
 * Android TV lets people name the device ("Living room TV"); fall back to the
 * model when they have not.
 */
private fun deviceName(context: Context): String =
    Settings.Global.getString(context.contentResolver, "device_name")?.takeIf { it.isNotBlank() }
        ?: "${Build.MANUFACTURER} ${Build.MODEL}".trim()
