package online.phimnet.tv.playback

import android.app.Application
import androidx.annotation.OptIn
import androidx.core.net.toUri
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.media3.common.C
import androidx.media3.common.Format
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.TrackSelectionOverride
import androidx.media3.common.Tracks
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.HttpDataSource
import androidx.media3.datasource.okhttp.OkHttpDataSource
import androidx.media3.exoplayer.DefaultLoadControl
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.source.DefaultMediaSourceFactory
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import online.phimnet.tv.AppGraph
import online.phimnet.tv.api.ApiException
import online.phimnet.tv.api.Episode
import online.phimnet.tv.api.Lock
import online.phimnet.tv.api.Prepared
import online.phimnet.tv.api.Stats
import online.phimnet.tv.api.Video
import online.phimnet.tv.api.Watch
import online.phimnet.tv.api.WatchKind
import online.phimnet.tv.api.lockKind
import java.util.Locale

/** One selectable audio or subtitle track. [id] is stable for the life of the media item. */
data class TrackChoice(val id: String, val label: String, val selected: Boolean)

/** A short-lived message over the video. [serial] makes a repeat of the same message re-show. */
data class Notice(val kind: Kind, val serial: Int) {
    enum class Kind { Compatibility, Reconnecting, SeekUnavailable }
}

/**
 * Plays one movie or episode, end to end:
 *
 *  1. Load the watch view-model (/api/tv/v1/watch/…) and pick a source with
 *     [QualityPicker] — the web page's own rules.
 *  2. prepare it (/api/sources/{id}/prepare). This is where the server ENFORCES
 *     the tiers; 401 / 402 / 403 become the sign-in / upgrade / coming-soon gates.
 *  3. Start the heartbeat straight away, then wait for the streamer to know the
 *     torrent's metadata ([awaitStreamReady]).
 *  4. Hand ExoPlayer the `raw=1` stream (see [StreamUrls]).
 *  5. On failure, [RecoveryPolicy] decides: prepare again (a reaped torrent), fall
 *     back to the transcode (a codec this device cannot decode), or give up.
 */
@OptIn(UnstableApi::class)
class PlayerViewModel(
    app: Application,
    private val graph: AppGraph,
    val kind: WatchKind,
    val ownerId: Long,
) : ViewModel() {

    sealed interface Phase {
        data object Loading : Phase
        data object Preparing : Phase
        data object Connecting : Phase
        data object Playing : Phase
        /** The title has no sources at all. */
        data object NoSource : Phase
        /** The torrent never produced metadata: no reachable seeders. */
        data object NoPeers : Phase
        /**
         * The chosen source is locked for this visitor. [alternative] is a source
         * they CAN play, offered instead, when there is one.
         */
        data class Gate(
            val video: Video,
            val lock: Lock,
            val message: String?,
            val alternative: Video?,
            /**
             * True when the gate was raised over a film that was playing (a locked
             * chip picked mid-watch). Then it behaves like a dialog: Back returns
             * to the paused film instead of leaving the player.
             */
            val overPlayback: Boolean = false,
        ) : Phase
        data class Failed(val error: Throwable) : Phase
        data object Ended : Phase
    }

    data class Ui(
        val phase: Phase = Phase.Loading,
        val watch: Watch? = null,
        val current: Video? = null,
        val compatibility: Boolean = false,
        val stats: Stats? = null,
        val subtitles: List<TrackChoice> = emptyList(),
        val audio: List<TrackChoice> = emptyList(),
        val notice: Notice? = null,
    ) {
        /** The episode after this one in the same season, for "next episode". */
        val next: Episode?
            get() {
                val w = watch ?: return null
                if (w.ownerKind != "episode") return null
                val sorted = w.episodes.sortedBy { it.number }
                val here = sorted.indexOfFirst { it.id == w.ownerId }
                return if (here >= 0) sorted.getOrNull(here + 1) else null
            }
    }

    private val _ui = MutableStateFlow(Ui())
    val ui: StateFlow<Ui> = _ui.asStateFlow()

    val player: ExoPlayer = ExoPlayer.Builder(app)
        .setMediaSourceFactory(
            DefaultMediaSourceFactory(
                // The streamer client: no viewer token on it, and a patient read
                // timeout for a swarm that pauses between pieces.
                OkHttpDataSource.Factory(graph.streamHttp).setUserAgent(graph.userAgent),
            ),
        )
        .setLoadControl(
            // A swarm delivers in bursts, so buffer more than the defaults do
            // before and during playback.
            DefaultLoadControl.Builder()
                .setBufferDurationsMs(30_000, 120_000, 2_500, 5_000)
                .build(),
        )
        .setSeekBackIncrementMs(SEEK_STEP_MS)
        .setSeekForwardIncrementMs(SEEK_STEP_MS)
        .build()

    private val session = WatchSession(
        beat = graph.api::heartbeat,
        leave = graph.api::leave,
        scope = viewModelScope,
        outlive = graph.appScope,
    )

    private val resumeKey = "${kind.path}:$ownerId"
    private var startJob: Job? = null
    private var reprepares = 0
    private var subtitlesApplied = false
    private var backgrounded = false
    private var noticeSerial = 0

    /**
     * Where the film was when a gate interrupted it. Pairing from that gate
     * reloads the whole screen (entitlements changed), and without this the
     * reload would restart the film from its last periodic save — or from zero.
     */
    private var handoffPosition: Long? = null

    private val listener = object : Player.Listener {
        override fun onPlaybackStateChanged(state: Int) {
            when (state) {
                Player.STATE_READY -> reprepares = 0 // it recovered; a later 404 gets a fresh budget
                Player.STATE_ENDED -> onEnded()
                else -> Unit
            }
        }

        override fun onTracksChanged(tracks: Tracks) = onTracks(tracks)

        override fun onPlayerError(error: PlaybackException) = onError(error)
    }

    init {
        player.addListener(listener)
        load()
        viewModelScope.launch { saveProgressPeriodically() }
    }

    // --- loading ----------------------------------------------------------------

    /** (Re)loads the watch view-model. Also called after pairing, since what unlocks may have changed. */
    fun load() {
        startJob?.cancel()
        startJob = viewModelScope.launch {
            _ui.update { it.copy(phase = Phase.Loading) }
            val watch = try {
                graph.api.watch(kind, ownerId)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _ui.update { it.copy(phase = Phase.Failed(e)) }
                return@launch
            }
            _ui.update { it.copy(watch = watch) }

            val remembered = graph.settings.current().quality
            val pick = QualityPicker.pick(watch.videos, remembered)
            when {
                watch.videos.isEmpty() -> _ui.update { it.copy(phase = Phase.NoSource) }
                pick == null -> {
                    val locked = QualityPicker.gate(watch.videos) ?: watch.videos.first()
                    _ui.update { it.copy(phase = Phase.Gate(locked, locked.lockKind, null, null)) }
                }
                else -> {
                    val resumeAt = handoffPosition ?: graph.settings.resumePosition(resumeKey)
                    handoffPosition = null
                    start(pick, resumeAt, compatibility = false)
                }
            }
        }
    }

    // --- starting a source --------------------------------------------------------

    /**
     * Starts [video], abandoning any start still in flight. A second press on a
     * quality chip while the first is still connecting must win, not queue.
     */
    private fun start(video: Video, resumeAt: Long, compatibility: Boolean) {
        startJob?.cancel()
        startJob = viewModelScope.launch { runStart(video, resumeAt, compatibility) }
    }

    private suspend fun runStart(video: Video, resumeAt: Long, compatibility: Boolean) {
        _ui.update { it.copy(phase = Phase.Preparing, current = video, compatibility = compatibility, stats = null) }

        val prepared = try {
            graph.api.prepare(video.id)
        } catch (e: CancellationException) {
            throw e
        } catch (e: ApiException) {
            // The server's verdict — the chips were only advisory.
            val lock = when (e.status) {
                401 -> Lock.Member
                402 -> Lock.Upgrade
                403 -> Lock.Paid
                else -> null
            }
            _ui.update {
                it.copy(
                    phase = if (lock != null) Phase.Gate(video, lock, e.message, alternativeTo(video)) else Phase.Failed(e),
                )
            }
            return
        } catch (e: Exception) {
            _ui.update { it.copy(phase = Phase.Failed(e)) }
            return
        }

        // Heartbeat BEFORE waiting: metadata can take longer than the viewer's
        // 30s session TTL to arrive, and the torrent must be claimed meanwhile.
        session.watch(prepared.infoHash)

        _ui.update { it.copy(phase = Phase.Connecting) }
        val ready = awaitStreamReady(
            poll = { graph.streamer.stats(prepared.streamerPublicURL, prepared.infoHash) },
            onUpdate = { stats -> _ui.update { it.copy(stats = stats) } },
        )
        if (!ready) {
            _ui.update { it.copy(phase = Phase.NoPeers) }
            return
        }

        subtitlesApplied = false
        player.setMediaItem(
            mediaItem(prepared, compatibility),
            // The transcode is sequential and cannot seek, so it always starts at 0.
            if (compatibility) 0L else resumeAt.coerceAtLeast(0L),
        )
        player.prepare()
        player.playWhenReady = !backgrounded
        _ui.update { it.copy(phase = Phase.Playing) }
    }

    private fun mediaItem(prepared: Prepared, compatibility: Boolean): MediaItem {
        val subtitles = _ui.value.watch?.subtitles.orEmpty().map { sub ->
            MediaItem.SubtitleConfiguration.Builder(graph.api.subtitleUrl(sub.id).toUri())
                .setId(SUBTITLE_ID_PREFIX + sub.id)
                .setMimeType(sub.mimeType())
                .setLanguage(sub.language.ifBlank { null })
                .setLabel(subtitleLabel(sub.language, sub.name.ifBlank { sub.provider }))
                .build()
        }
        return MediaItem.Builder()
            .setUri(StreamUrls.stream(prepared, if (compatibility) StreamMode.Compatibility else StreamMode.Direct))
            .setSubtitleConfigurations(subtitles)
            .build()
    }

    private fun alternativeTo(video: Video): Video? =
        QualityPicker.pick(_ui.value.watch?.videos.orEmpty().filter { it.id != video.id }, null)

    // --- choices the person makes -----------------------------------------------------

    /**
     * A source picked by hand. Only this remembers the quality — never automatic
     * playback, or one title without a 4K source would quietly demote the person
     * for every title after it (the web page's chooseSource/playSource split).
     */
    fun chooseSource(video: Video) {
        if (!video.available) {
            val wasPlaying = _ui.value.phase == Phase.Playing
            if (wasPlaying) {
                // A locked chip mid-film: pause under the gate rather than playing on
                // behind it, and remember the place for whatever comes next.
                player.pause()
                if (!_ui.value.compatibility) handoffPosition = player.currentPosition
            }
            _ui.update {
                it.copy(phase = Phase.Gate(video, video.lockKind, null, alternativeTo(video), overPlayback = wasPlaying))
            }
            return
        }
        viewModelScope.launch { graph.settings.setQuality(video.resolution) }
        if (video.id == _ui.value.current?.id && _ui.value.phase == Phase.Playing) return
        // Keep the place when switching quality — unless we were on the
        // transcode, whose position is not a seekable point in the real file.
        val position = if (_ui.value.compatibility) 0L else player.currentPosition
        start(video, position, compatibility = false)
    }

    fun chooseSubtitle(id: String?) = selectTrack(C.TRACK_TYPE_TEXT, id)

    fun chooseAudio(id: String) = selectTrack(C.TRACK_TYPE_AUDIO, id)

    /** Back from a gate raised over a playing film: return to the film, paused where it was. */
    fun dismissGate() {
        val gate = _ui.value.phase as? Phase.Gate ?: return
        if (!gate.overPlayback) return
        handoffPosition = null
        _ui.update { it.copy(phase = Phase.Playing) }
    }

    fun togglePlay() {
        if (player.isPlaying) player.pause() else player.play()
    }

    /** Seeks by [deltaMs]. The transcode cannot seek, so there it only explains why. */
    fun seekBy(deltaMs: Long) {
        if (_ui.value.compatibility) {
            notify(Notice.Kind.SeekUnavailable)
            return
        }
        val duration = player.duration.takeIf { it != C.TIME_UNSET } ?: Long.MAX_VALUE
        player.seekTo((player.currentPosition + deltaMs).coerceIn(0L, duration))
    }

    fun dismissNotice(serial: Int) {
        _ui.update { if (it.notice?.serial == serial) it.copy(notice = null) else it }
    }

    // --- lifecycle ------------------------------------------------------------------

    /**
     * The app went to the background (Home pressed, screensaver). Stop claiming
     * the torrent at once so the streamer can drop it, and remember the place.
     */
    fun onBackground() {
        if (backgrounded) return
        backgrounded = true
        player.pause()
        persistPosition()
        session.end()
    }

    /** Back again: the torrent may have been dropped meanwhile, so prepare it afresh. */
    fun onForeground() {
        if (!backgrounded) return
        backgrounded = false
        val video = _ui.value.current ?: return
        if (_ui.value.phase != Phase.Playing) return
        start(video, player.currentPosition, _ui.value.compatibility)
    }

    override fun onCleared() {
        persistPosition()
        session.end()
        player.removeListener(listener)
        player.release()
    }

    // --- player events ------------------------------------------------------------

    private fun onTracks(tracks: Tracks) {
        // A file whose every audio (or video) track this device cannot decode
        // plays silently (or black) rather than failing. Treat it like a decoder
        // error and fall back to the transcode, once.
        val undecodable = listOf(C.TRACK_TYPE_AUDIO, C.TRACK_TYPE_VIDEO).any { type ->
            val groups = tracks.groups.filter { it.type == type }
            groups.isNotEmpty() && groups.none { it.isSupported }
        }
        if (undecodable && !_ui.value.compatibility) {
            fallBackToCompatibility()
            return
        }

        // Auto-apply the first SAVED subtitle once per source, as the web page
        // auto-loads its first chip. Later changes are the person's own.
        if (!subtitlesApplied) {
            val first = _ui.value.watch?.subtitles?.firstOrNull()
            val choice = first?.let { sub ->
                choices(tracks, C.TRACK_TYPE_TEXT).firstOrNull { it.id.contains(SUBTITLE_ID_PREFIX + sub.id) }
            }
            if (choice != null) {
                subtitlesApplied = true
                selectTrack(C.TRACK_TYPE_TEXT, choice.id)
                // Do NOT return here waiting for the next onTracksChanged: if this
                // selection is already in effect (a reload after signing in keeps the
                // same parameters) no further event comes, and the subtitle and
                // audio rows would stay empty. Publish now; a real change re-publishes.
            }
        }

        _ui.update {
            it.copy(
                subtitles = choices(tracks, C.TRACK_TYPE_TEXT),
                audio = choices(tracks, C.TRACK_TYPE_AUDIO),
            )
        }
    }

    private fun onError(error: PlaybackException) {
        val video = _ui.value.current ?: return
        val status = generateSequence<Throwable>(error) { it.cause }
            .filterIsInstance<HttpDataSource.InvalidResponseCodeException>()
            .firstOrNull()?.responseCode
        when (RecoveryPolicy.classify(error.errorCode, status, _ui.value.compatibility, reprepares)) {
            Recovery.Reprepare -> {
                reprepares++
                notify(Notice.Kind.Reconnecting)
                val position = if (_ui.value.compatibility) 0L else player.currentPosition
                start(video, position, _ui.value.compatibility)
            }
            Recovery.Compatibility -> fallBackToCompatibility()
            Recovery.Fail -> _ui.update { it.copy(phase = Phase.Failed(error)) }
        }
    }

    private fun fallBackToCompatibility() {
        val video = _ui.value.current ?: return
        notify(Notice.Kind.Compatibility)
        start(video, 0L, compatibility = true)
    }

    private fun onEnded() {
        viewModelScope.launch { graph.settings.clearResumePosition(resumeKey) }
        session.end()
        _ui.update { it.copy(phase = Phase.Ended) }
    }

    // --- tracks -------------------------------------------------------------------

    private fun choices(tracks: Tracks, type: Int): List<TrackChoice> =
        tracks.groups.withIndex()
            .filter { (_, group) -> group.type == type }
            .flatMap { (g, group) ->
                (0 until group.length).filter { group.isTrackSupported(it) }.map { t ->
                    val format = group.getTrackFormat(t)
                    TrackChoice(trackId(format, g, t), trackLabel(format, type), group.isTrackSelected(t))
                }
            }

    private fun selectTrack(type: Int, id: String?) {
        val params = player.trackSelectionParameters.buildUpon()
        if (id == null) {
            params.setTrackTypeDisabled(type, true)
        } else {
            val match = player.currentTracks.groups.withIndex().firstNotNullOfOrNull { (g, group) ->
                if (group.type != type) return@firstNotNullOfOrNull null
                (0 until group.length).firstOrNull { trackId(group.getTrackFormat(it), g, it) == id }
                    ?.let { group to it }
            } ?: return
            params.setTrackTypeDisabled(type, false)
            params.setOverrideForType(TrackSelectionOverride(match.first.mediaTrackGroup, match.second))
        }
        player.trackSelectionParameters = params.build()
    }

    private fun trackId(format: Format, group: Int, track: Int): String = format.id ?: "g$group:t$track"

    private fun trackLabel(format: Format, type: Int): String {
        val language = format.language?.let { languageName(it) }
        val label = format.label
        return when {
            label != null && language != null && !label.startsWith(language) -> "$language · $label"
            label != null -> label
            language != null -> language
            type == C.TRACK_TYPE_AUDIO && format.channelCount > 0 -> "${format.channelCount}ch"
            else -> "#${format.id ?: "?"}"
        }
    }

    private fun subtitleLabel(language: String, name: String): String {
        val lang = languageName(language)
        return when {
            lang == null -> name
            name.isBlank() -> lang
            else -> "$lang · $name"
        }
    }

    /** A language's name in the app's own language ("vi" → "Tiếng Việt" or "Vietnamese"). */
    private fun languageName(tag: String): String? {
        if (tag.isBlank() || tag == C.LANGUAGE_UNDETERMINED) return null
        val name = Locale.forLanguageTag(tag).getDisplayLanguage(Locale.getDefault())
        return name.takeIf { it.isNotBlank() && it != tag }?.replaceFirstChar { it.titlecase(Locale.getDefault()) } ?: tag
    }

    // --- progress -------------------------------------------------------------------

    /** Runs until the ViewModel is cleared, which cancels it inside delay(). */
    private suspend fun saveProgressPeriodically() {
        while (true) {
            delay(10_000)
            if (_ui.value.phase == Phase.Playing && player.isPlaying) persistPosition()
        }
    }

    /**
     * Remembers where the person is. Near the very end counts as finished, so the
     * next visit starts from the beginning rather than on the credits.
     */
    private fun persistPosition() {
        if (_ui.value.phase != Phase.Playing) return
        val position = player.currentPosition
        val duration = player.duration.takeIf { it != C.TIME_UNSET }
        graph.appScope.launch {
            when {
                duration != null && position > duration - FINISHED_MARGIN_MS -> graph.settings.clearResumePosition(resumeKey)
                position > MIN_RESUME_MS -> graph.settings.saveResumePosition(resumeKey, position)
            }
        }
    }

    private fun notify(kind: Notice.Kind) {
        noticeSerial++
        _ui.update { it.copy(notice = Notice(kind, noticeSerial)) }
    }

    companion object {
        /** Same as the web page's SEEK_STEP. */
        const val SEEK_STEP_MS = 10_000L
        private const val SUBTITLE_ID_PREFIX = "phimnet-sub-"
        private const val MIN_RESUME_MS = 30_000L
        private const val FINISHED_MARGIN_MS = 60_000L
    }
}
