package online.phimnet.tv.ui.settings

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.tv.material3.Button
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.OutlinedButton
import androidx.tv.material3.Text
import kotlinx.coroutines.launch
import online.phimnet.tv.BuildConfig
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.settings.ServerUrl
import online.phimnet.tv.settings.SiteLocale
import online.phimnet.tv.ui.common.ChoiceChip
import online.phimnet.tv.ui.common.SectionTitle
import online.phimnet.tv.ui.common.Tab
import online.phimnet.tv.ui.common.TopBar
import online.phimnet.tv.ui.common.UpdatePanel
import online.phimnet.tv.ui.common.dpadLeavesTextField
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.theme.Accent
import online.phimnet.tv.ui.theme.Surface2
import online.phimnet.tv.ui.theme.TextMuted

@Composable
fun SettingsScreen(onTab: (Tab) -> Unit, onSignIn: () -> Unit) {
    val graph = LocalGraph.current
    val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
    val scope = rememberCoroutineScope()
    val current = prefs ?: return

    // Signing in or out swaps one account button for another. The focused button
    // disappears, and on a TV that leaves NOTHING focused — so follow the change
    // to whichever button now exists. Not on first appearance: then the tab bar
    // owns focus.
    val accountButton = remember { FocusRequester() }
    val signedIn = current.token != null
    var lastSignedIn by remember { mutableStateOf(signedIn) }
    LaunchedEffect(signedIn) {
        if (signedIn != lastSignedIn) {
            lastSignedIn = signedIn
            accountButton.focusWhenReady()
        }
    }

    // A token the server no longer honours (revoked from the account page, or
    // the server's secret rotated) reads as signed out on /me. Forget it here,
    // so the screen never claims a sign-in the server has already ended.
    LaunchedEffect(current.token, current.serverUrl) {
        if (current.token == null) return@LaunchedEffect
        val me = runCatching { graph.api.me() }.getOrNull() ?: return@LaunchedEffect
        if (!me.signedIn) graph.settings.signOut()
    }

    Column(Modifier.fillMaxSize()) {
        TopBar(current = Tab.Account, onSelect = onTab, focusCurrent = true)
        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 48.dp, vertical = 16.dp),
            verticalArrangement = Arrangement.spacedBy(36.dp),
        ) {
            // Account.
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                SectionTitle(stringResource(R.string.nav_account))
                if (current.token != null) {
                    Text(stringResource(R.string.settings_signed_in_as, current.userName.orEmpty()))
                    OutlinedButton(modifier = Modifier.focusRequester(accountButton), onClick = {
                        scope.launch {
                            // Revoke on the server first, so the token is dead rather
                            // than merely forgotten. Best effort: offline, the local
                            // sign-out still happens, and the pairing can be removed
                            // from the account page.
                            runCatching { graph.api.deviceLogout() }
                            graph.settings.signOut()
                        }
                    }) { Text(stringResource(R.string.action_sign_out)) }
                } else {
                    Text(stringResource(R.string.player_free_hint), color = TextMuted)
                    Button(onClick = onSignIn, modifier = Modifier.focusRequester(accountButton)) {
                        Text(stringResource(R.string.action_sign_in))
                    }
                }
            }

            // Content language.
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                SectionTitle(stringResource(R.string.settings_language))
                LazyRow(
                    modifier = Modifier.focusRestorer(),
                    horizontalArrangement = Arrangement.spacedBy(10.dp),
                    contentPadding = PaddingValues(vertical = 4.dp),
                ) {
                    item(key = "auto") {
                        ChoiceChip(
                            selected = current.localeOverride == null,
                            onClick = { scope.launch { graph.settings.setLocaleOverride(null) } },
                        ) { Text(stringResource(R.string.settings_language_auto)) }
                    }
                    items(SiteLocale.SUPPORTED, key = { it }) { code ->
                        ChoiceChip(
                            selected = current.localeOverride == code,
                            onClick = { scope.launch { graph.settings.setLocaleOverride(code) } },
                        ) { Text(SiteLocale.nativeName(code)) }
                    }
                }
            }

            ServerSection(current.serverUrl) { url -> scope.launch { graph.settings.setServerUrl(url) } }

            // The app itself: which version this is, and updates. Sideloaded, so
            // this is the only place (with the Home banner) that ever updates it.
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                SectionTitle(stringResource(R.string.settings_app))
                Text(stringResource(R.string.settings_version, BuildConfig.VERSION_NAME))
                val update by graph.updater.state.collectAsStateWithLifecycle()
                UpdatePanel(update, inSettings = true)
            }
        }
    }
}

@Composable
private fun ServerSection(saved: String, onSave: (String) -> Unit) {
    var text by remember(saved) { mutableStateOf(saved) }
    var problem by remember { mutableStateOf<Int?>(null) }
    var focused by remember { mutableStateOf(false) }

    fun save() {
        when (val result = ServerUrl.parse(text, BuildConfig.ALLOW_CLEARTEXT)) {
            is ServerUrl.Result.Ok -> {
                problem = null
                val url = ServerUrl.format(result.url)
                text = url
                if (url != saved) onSave(url)
            }
            ServerUrl.Result.Invalid -> problem = R.string.settings_server_invalid
            ServerUrl.Result.CleartextNotAllowed -> problem = R.string.settings_server_https
        }
    }

    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        SectionTitle(stringResource(R.string.settings_server))
        Text(stringResource(R.string.settings_server_hint), color = TextMuted)
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Box(
                Modifier
                    .width(560.dp)
                    .background(Surface2, RoundedCornerShape(8.dp))
                    .border(BorderStroke(2.dp, if (focused) Color.White else Color.Transparent), RoundedCornerShape(8.dp))
                    .padding(horizontal = 20.dp, vertical = 14.dp),
            ) {
                BasicTextField(
                    value = text,
                    onValueChange = { text = it; problem = null },
                    singleLine = true,
                    textStyle = MaterialTheme.typography.titleMedium.copy(color = Color.White),
                    cursorBrush = SolidColor(Color.White),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Done),
                    keyboardActions = KeyboardActions(onDone = { save() }),
                    modifier = Modifier.dpadLeavesTextField().onFocusChanged { focused = it.isFocused },
                )
            }
            Button(onClick = { save() }) { Text(stringResource(R.string.action_save)) }
        }
        problem?.let { Text(stringResource(it), color = Accent) }
        Spacer(Modifier.height(0.dp))
        Text(stringResource(R.string.settings_server_note), color = TextMuted, style = MaterialTheme.typography.bodySmall)
    }
}
