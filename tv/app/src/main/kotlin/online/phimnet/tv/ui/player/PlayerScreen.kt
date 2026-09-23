package online.phimnet.tv.ui.player

import android.app.Application
import android.view.ViewGroup
import androidx.activity.compose.BackHandler
import androidx.annotation.OptIn
import androidx.compose.foundation.background
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.media3.common.C
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Button
import androidx.tv.material3.Icon
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.OutlinedButton
import androidx.tv.material3.Text
import kotlinx.coroutines.delay
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.api.Lock
import online.phimnet.tv.api.Video
import online.phimnet.tv.api.WatchKind
import online.phimnet.tv.playback.Notice
import online.phimnet.tv.playback.PlayerViewModel
import online.phimnet.tv.playback.PlayerViewModel.Phase
import online.phimnet.tv.playback.TrackChoice
import online.phimnet.tv.ui.common.ChoiceChip
import online.phimnet.tv.ui.common.QrCode
import online.phimnet.tv.ui.common.errorText
import online.phimnet.tv.ui.common.focusWhenReady
import online.phimnet.tv.ui.common.rememberSessionKey
import online.phimnet.tv.ui.theme.Accent
import online.phimnet.tv.ui.theme.TextMuted

@Composable
fun PlayerScreen(
    kind: WatchKind,
    id: Long,
    onBack: () -> Unit,
    onSignIn: () -> Unit,
    onPlayEpisode: (Long) -> Unit,
) {
    val graph = LocalGraph.current
    val app = LocalContext.current.applicationContext as Application
    val vm: PlayerViewModel = viewModel(key = "player:${kind.path}:$id") { PlayerViewModel(app, graph, kind, id) }
    val ui by vm.ui.collectAsStateWithLifecycle()

    // Pairing on the sign-in screen changes what this person may play, so come
    // back to a fresh answer rather than the gate they left.
    val token = rememberSessionKey().token
    var seenToken by rememberSaveable { mutableStateOf(token) }
    LaunchedEffect(token) {
        if (token != seenToken) {
            seenToken = token
            vm.load()
        }
    }

    // Home button / screensaver: release the torrent now; prepare it afresh on return.
    LifecycleEventEffect(Lifecycle.Event.ON_STOP) { vm.onBackground() }
    LifecycleEventEffect(Lifecycle.Event.ON_START) { vm.onForeground() }

    Box(Modifier.fillMaxSize().background(Color.Black)) {
        VideoSurface(vm.player)
        when (val phase = ui.phase) {
            Phase.Playing -> PlayingControls(vm, ui, onPlayEpisode)
            Phase.Loading, Phase.Preparing, Phase.Connecting -> Connecting(ui)
            is Phase.Gate -> GatePanel(phase, ui.watch?.upgradeUrl.orEmpty(), vm, onSignIn, onBack)
            Phase.NoSource -> MessagePanel(stringResource(R.string.player_no_source), null, onBack)
            Phase.NoPeers -> MessagePanel(stringResource(R.string.player_no_peers), vm::load, onBack)
            is Phase.Failed -> MessagePanel(errorText(phase.error), vm::load, onBack)
            Phase.Ended -> EndedPanel(ui, onPlayEpisode, onBack)
        }
        ui.notice?.let { NoticeToast(it, vm::dismissNotice) }
    }
}

@OptIn(UnstableApi::class)
@Composable
private fun VideoSurface(player: Player) {
    AndroidView(
        factory = { context ->
            PlayerView(context).apply {
                useController = false
                this.player = player
                keepScreenOn = true
                setShowBuffering(PlayerView.SHOW_BUFFERING_WHEN_PLAYING)
                // Compose owns the remote. The view must never take focus, or the
                // D-pad would drive ExoPlayer's own (hidden) controller instead.
                isFocusable = false
                descendantFocusability = ViewGroup.FOCUS_BLOCK_DESCENDANTS
            }
        },
        onRelease = { it.player = null },
        modifier = Modifier.fillMaxSize(),
    )
}

// --- playing ---------------------------------------------------------------------------

@Immutable
private data class Progress(
    val position: Long = 0,
    val duration: Long = 0,
    val buffered: Long = 0,
    val playing: Boolean = false,
)

@Composable
private fun rememberProgress(player: Player): Progress {
    val progress by produceState(Progress(), player) {
        while (true) {
            value = Progress(
                position = player.currentPosition,
                duration = player.duration.takeIf { it != C.TIME_UNSET } ?: 0,
                buffered = player.bufferedPosition,
                playing = player.isPlaying,
            )
            delay(500)
        }
    }
    return progress
}

@Composable
private fun PlayingControls(vm: PlayerViewModel, ui: PlayerViewModel.Ui, onPlayEpisode: (Long) -> Unit) {
    val progress = rememberProgress(vm.player)
    var visible by remember { mutableStateOf(true) }
    var input by remember { mutableIntStateOf(0) }
    var seekFlash by remember { mutableIntStateOf(0) }
    val root = remember { FocusRequester() }
    val playButton = remember { FocusRequester() }

    // Hide after a few idle seconds while playing; stay up while paused.
    LaunchedEffect(visible, input, progress.playing) {
        if (visible && progress.playing) {
            delay(5_000)
            visible = false
        }
    }
    LaunchedEffect(visible) {
        if (visible) playButton.focusWhenReady() else root.focusWhenReady()
    }
    BackHandler(enabled = visible) { visible = false }

    Box(
        Modifier
            .fillMaxSize()
            .focusRequester(root)
            .focusable()
            .onPreviewKeyEvent { event ->
                if (event.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                input++
                when (event.key) {
                    // Media keys work whatever is on screen.
                    Key.MediaPlayPause, Key.MediaPlay, Key.MediaPause -> { vm.togglePlay(); visible = true; true }
                    Key.MediaFastForward -> { vm.seekBy(PlayerViewModel.SEEK_STEP_MS); seekFlash++; true }
                    Key.MediaRewind -> { vm.seekBy(-PlayerViewModel.SEEK_STEP_MS); seekFlash++; true }
                    // With the controls hidden, the D-pad drives playback directly —
                    // the same keys the web page binds (space, ←/→ 10s).
                    Key.DirectionCenter, Key.Enter, Key.NumPadEnter -> if (!visible) { vm.togglePlay(); visible = true; true } else false
                    Key.DirectionLeft -> if (!visible) { vm.seekBy(-PlayerViewModel.SEEK_STEP_MS); seekFlash++; true } else false
                    Key.DirectionRight -> if (!visible) { vm.seekBy(PlayerViewModel.SEEK_STEP_MS); seekFlash++; true } else false
                    Key.DirectionUp, Key.DirectionDown -> if (!visible) { visible = true; true } else false
                    else -> false
                }
            },
    ) {
        if (visible) {
            Controls(vm, ui, progress, playButton, onPlayEpisode)
        } else if (seekFlash > 0) {
            SeekFlash(progress, seekFlash)
        }
    }
}

@Composable
private fun Controls(
    vm: PlayerViewModel,
    ui: PlayerViewModel.Ui,
    progress: Progress,
    playButton: FocusRequester,
    onPlayEpisode: (Long) -> Unit,
) {
    val watch = ui.watch ?: return
    Box(Modifier.fillMaxSize()) {
        // Top: what is playing.
        Column(
            Modifier
                .fillMaxWidth()
                .background(Brush.verticalGradient(listOf(Color(0xCC000000), Color.Transparent)))
                .padding(horizontal = 48.dp, vertical = 28.dp),
        ) {
            Text(watch.heading, style = MaterialTheme.typography.headlineMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (watch.sub.isNotBlank()) Text(watch.sub, color = TextMuted, style = MaterialTheme.typography.titleMedium)
        }
        // Bottom: the timeline and every choice.
        Column(
            Modifier
                .align(Alignment.BottomStart)
                .fillMaxWidth()
                .background(Brush.verticalGradient(listOf(Color.Transparent, Color(0xE6000000))))
                .padding(horizontal = 48.dp, vertical = 28.dp),
            verticalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            Timeline(progress)
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                Button(onClick = vm::togglePlay, modifier = Modifier.focusRequester(playButton)) {
                    Icon(
                        painterResource(if (progress.playing) R.drawable.ic_pause else R.drawable.ic_play),
                        contentDescription = stringResource(R.string.player_play_pause),
                    )
                }
                OutlinedButton(onClick = { vm.seekBy(-PlayerViewModel.SEEK_STEP_MS) }) { Text("−10s") }
                OutlinedButton(onClick = { vm.seekBy(PlayerViewModel.SEEK_STEP_MS) }) { Text("+10s") }
                ui.next?.let { next ->
                    Spacer(Modifier.width(12.dp))
                    OutlinedButton(onClick = { onPlayEpisode(next.id) }) {
                        Text(
                            stringResource(R.string.player_next_episode) + " · " +
                                next.name.ifBlank { stringResource(R.string.detail_episode, next.number) },
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                            modifier = Modifier.widthIn(max = 360.dp),
                        )
                    }
                }
            }
            ChoiceRow(
                title = stringResource(R.string.player_sources),
                items = watch.videos,
                key = { it.id },
                label = { v -> v.resolution },
                selected = { it.id == ui.current?.id },
                locked = { !it.available },
                onPick = vm::chooseSource,
            )
            if (ui.subtitles.isNotEmpty()) {
                val off = TrackChoice("", stringResource(R.string.player_subtitles_off), ui.subtitles.none { it.selected })
                ChoiceRow(
                    title = stringResource(R.string.player_subtitles),
                    items = listOf(off) + ui.subtitles,
                    key = { it.id },
                    label = { it.label },
                    selected = { it.selected },
                    onPick = { vm.chooseSubtitle(it.id.ifEmpty { null }) },
                )
            }
            // Many releases carry a dub and the original; only offer a choice when there is one.
            if (ui.audio.size > 1) {
                ChoiceRow(
                    title = stringResource(R.string.player_audio),
                    items = ui.audio,
                    key = { it.id },
                    label = { it.label },
                    selected = { it.selected },
                    onPick = { vm.chooseAudio(it.id) },
                )
            }
        }
    }
}

@Composable
private fun <T> ChoiceRow(
    title: String,
    items: List<T>,
    key: (T) -> Any,
    label: (T) -> String,
    selected: (T) -> Boolean,
    onPick: (T) -> Unit,
    locked: (T) -> Boolean = { false },
) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Text(title, modifier = Modifier.width(140.dp), color = TextMuted, style = MaterialTheme.typography.labelLarge)
        LazyRow(
            modifier = Modifier.focusRestorer(),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
            contentPadding = PaddingValues(vertical = 4.dp),
        ) {
            items(items, key = key) { item ->
                ChoiceChip(
                    selected = selected(item),
                    onClick = { onPick(item) },
                    leading = if (locked(item)) {
                        { Icon(painterResource(R.drawable.ic_lock), contentDescription = null, modifier = Modifier.size(16.dp)) }
                    } else null,
                ) {
                    // Long release names are clipped rather than allowed to push the
                    // row off screen; short ones ("720p") keep their natural width.
                    Text(label(item), maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.widthIn(max = 240.dp))
                }
            }
        }
    }
}

@Composable
private fun Timeline(progress: Progress) {
    val duration = progress.duration.coerceAtLeast(1)
    Column {
        Box(
            Modifier
                .fillMaxWidth()
                .height(6.dp)
                .clip(RoundedCornerShape(3.dp))
                .background(Color(0x33FFFFFF)),
        ) {
            Box(
                Modifier
                    .fillMaxHeight()
                    .fillMaxWidth((progress.buffered.toFloat() / duration).coerceIn(0f, 1f))
                    .background(Color(0x55FFFFFF)),
            )
            Box(
                Modifier
                    .fillMaxHeight()
                    .fillMaxWidth((progress.position.toFloat() / duration).coerceIn(0f, 1f))
                    .background(Accent),
            )
        }
        Spacer(Modifier.height(6.dp))
        Text(
            "${formatTime(progress.position)} / ${formatTime(progress.duration)}",
            style = MaterialTheme.typography.labelLarge,
            color = TextMuted,
        )
    }
}

/** A brief time readout after a seek with the controls hidden. */
@Composable
private fun SeekFlash(progress: Progress, serial: Int) {
    var shown by remember { mutableStateOf(true) }
    LaunchedEffect(serial) {
        shown = true
        delay(1_500)
        shown = false
    }
    if (!shown) return
    Box(Modifier.fillMaxSize().padding(bottom = 64.dp), contentAlignment = Alignment.BottomCenter) {
        Text(
            "${formatTime(progress.position)} / ${formatTime(progress.duration)}",
            modifier = Modifier
                .clip(RoundedCornerShape(20.dp))
                .background(Color(0xCC000000))
                .padding(horizontal = 20.dp, vertical = 8.dp),
            style = MaterialTheme.typography.titleMedium,
        )
    }
}

// --- the moments before and after playing ---------------------------------------------------

@Composable
private fun Connecting(ui: PlayerViewModel.Ui) {
    Column(
        Modifier.fillMaxSize().padding(96.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        ui.watch?.let {
            Text(it.heading, style = MaterialTheme.typography.headlineMedium, textAlign = TextAlign.Center)
            if (it.sub.isNotBlank()) Text(it.sub, color = TextMuted, style = MaterialTheme.typography.titleMedium)
            Spacer(Modifier.height(32.dp))
        }
        Text(
            stringResource(if (ui.phase == Phase.Connecting) R.string.player_connecting else R.string.player_preparing),
            style = MaterialTheme.typography.titleLarge,
        )
        ui.current?.let { Text(it.resolution, color = TextMuted, style = MaterialTheme.typography.labelLarge) }
        Spacer(Modifier.height(12.dp))
        if (ui.phase == Phase.Connecting) {
            Text(
                stringResource(R.string.player_notice),
                color = TextMuted,
                textAlign = TextAlign.Center,
                modifier = Modifier.width(640.dp),
            )
            // Peer counts only. The streamer's speed figures are shared between
            // every client polling the same torrent and would read as nonsense.
            val peers = ui.stats?.activePeers ?: 0
            if (peers > 0) {
                Spacer(Modifier.height(12.dp))
                Text(stringResource(R.string.player_peers, peers), color = TextMuted)
            }
        }
    }
}

/**
 * Why this source cannot play, and what would change that. The copy comes from
 * the lock the server assigned — the app knows no rule of its own.
 */
@Composable
private fun GatePanel(
    gate: Phase.Gate,
    upgradeUrl: String,
    vm: PlayerViewModel,
    onSignIn: () -> Unit,
    onBack: () -> Unit,
) {
    val graph = LocalGraph.current
    val primary = remember { FocusRequester() }
    val resolution = gate.video.resolution
    // Over a paused film the gate is a dialog: Back returns to the film.
    val back: () -> Unit = if (gate.overPlayback) vm::dismissGate else onBack
    BackHandler(enabled = gate.overPlayback) { vm.dismissGate() }
    val showQr = gate.lock == Lock.Upgrade && upgradeUrl.isNotEmpty()

    Row(
        Modifier.fillMaxSize().background(Color(0xF0141414)).padding(96.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(48.dp),
    ) {
        Column(Modifier.weight(1f)) {
            Text(
                when (gate.lock) {
                    Lock.Member -> stringResource(R.string.player_sign_in_quality, resolution)
                    Lock.Upgrade -> stringResource(R.string.player_upgrade_quality, resolution)
                    else -> stringResource(R.string.player_coming_soon)
                },
                style = MaterialTheme.typography.headlineMedium,
            )
            Spacer(Modifier.height(12.dp))
            Text(
                when (gate.lock) {
                    Lock.Member -> stringResource(R.string.player_free_hint)
                    Lock.Upgrade -> stringResource(R.string.player_crypto_hint)
                    else -> gate.message.orEmpty()
                },
                color = TextMuted,
                style = MaterialTheme.typography.bodyLarge,
            )
            Spacer(Modifier.height(28.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                when {
                    gate.lock == Lock.Member ->
                        Button(onClick = onSignIn, modifier = Modifier.focusRequester(primary)) {
                            Text(stringResource(R.string.action_sign_in))
                        }
                    gate.alternative != null ->
                        Button(onClick = { vm.chooseSource(gate.alternative) }, modifier = Modifier.focusRequester(primary)) {
                            Text(stringResource(R.string.player_other_quality, gate.alternative.resolution))
                        }
                    else ->
                        Button(onClick = back, modifier = Modifier.focusRequester(primary)) {
                            Text(stringResource(R.string.action_back))
                        }
                }
                if (gate.lock == Lock.Member && gate.alternative != null) {
                    OutlinedButton(onClick = { vm.chooseSource(gate.alternative) }) {
                        Text(stringResource(R.string.player_other_quality, gate.alternative.resolution))
                    }
                }
                if (gate.lock == Lock.Member || gate.alternative != null) {
                    OutlinedButton(onClick = back) { Text(stringResource(R.string.action_back)) }
                }
            }
        }
        if (showQr) {
            Column(horizontalAlignment = Alignment.CenterHorizontally) {
                QrCode(graph.api.absolute(upgradeUrl), 240.dp)
                Spacer(Modifier.height(12.dp))
                Text(stringResource(R.string.player_scan_to_upgrade), color = TextMuted, textAlign = TextAlign.Center)
            }
        }
    }
    LaunchedEffect(gate) { primary.focusWhenReady() }
}

@Composable
private fun MessagePanel(message: String, onRetry: (() -> Unit)?, onBack: () -> Unit) {
    val primary = remember { FocusRequester() }
    Column(
        Modifier.fillMaxSize().background(Color(0xF0141414)).padding(96.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(message, style = MaterialTheme.typography.titleLarge, textAlign = TextAlign.Center, modifier = Modifier.width(720.dp))
        Spacer(Modifier.height(28.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            if (onRetry != null) {
                Button(onClick = onRetry, modifier = Modifier.focusRequester(primary)) { Text(stringResource(R.string.action_retry)) }
                OutlinedButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
            } else {
                Button(onClick = onBack, modifier = Modifier.focusRequester(primary)) { Text(stringResource(R.string.action_back)) }
            }
        }
    }
    LaunchedEffect(message) { primary.focusWhenReady() }
}

@Composable
private fun EndedPanel(ui: PlayerViewModel.Ui, onPlayEpisode: (Long) -> Unit, onBack: () -> Unit) {
    val primary = remember { FocusRequester() }
    val next = ui.next
    Column(
        Modifier.fillMaxSize().background(Color(0xE6141414)).padding(96.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        if (next != null) {
            Text(stringResource(R.string.player_up_next), color = TextMuted, style = MaterialTheme.typography.titleMedium)
            Spacer(Modifier.height(8.dp))
            Text(
                "${next.number}. ${next.name.ifBlank { stringResource(R.string.detail_episode, next.number) }}",
                style = MaterialTheme.typography.headlineMedium,
                textAlign = TextAlign.Center,
            )
            Spacer(Modifier.height(28.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Button(onClick = { onPlayEpisode(next.id) }, modifier = Modifier.focusRequester(primary)) {
                    Text(stringResource(R.string.player_next_episode))
                }
                OutlinedButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
            }
        } else {
            Text(stringResource(R.string.player_ended), style = MaterialTheme.typography.headlineMedium)
            Spacer(Modifier.height(28.dp))
            Button(onClick = onBack, modifier = Modifier.focusRequester(primary)) { Text(stringResource(R.string.action_back)) }
        }
    }
    LaunchedEffect(next) { primary.focusWhenReady() }
}

@Composable
private fun NoticeToast(notice: Notice, dismiss: (Int) -> Unit) {
    LaunchedEffect(notice.serial) {
        delay(4_000)
        dismiss(notice.serial)
    }
    Box(Modifier.fillMaxSize().padding(top = 36.dp), contentAlignment = Alignment.TopCenter) {
        Text(
            stringResource(
                when (notice.kind) {
                    Notice.Kind.Compatibility -> R.string.player_compat
                    Notice.Kind.Reconnecting -> R.string.player_reconnecting
                    Notice.Kind.SeekUnavailable -> R.string.player_seek_unavailable
                },
            ),
            modifier = Modifier
                .clip(RoundedCornerShape(20.dp))
                .background(Color(0xE6000000))
                .padding(horizontal = 24.dp, vertical = 10.dp),
            style = MaterialTheme.typography.titleSmall,
        )
    }
}

private fun formatTime(ms: Long): String {
    val total = (ms / 1000).coerceAtLeast(0)
    val h = total / 3600
    val m = (total % 3600) / 60
    val s = total % 60
    return if (h > 0) "%d:%02d:%02d".format(h, m, s) else "%d:%02d".format(m, s)
}
