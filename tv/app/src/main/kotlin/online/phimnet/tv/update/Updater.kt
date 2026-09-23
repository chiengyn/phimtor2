package online.phimnet.tv.update

import android.app.PendingIntent
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import android.util.Log
import androidx.core.net.toUri
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.HttpUrl
import online.phimnet.tv.api.AppRelease
import online.phimnet.tv.api.PhimnetApi
import java.io.File

sealed interface UpdateState {
    /** Not checked yet this launch. */
    data object Idle : UpdateState
    data object Checking : UpdateState
    data object UpToDate : UpdateState
    data class Available(val release: AppRelease) : UpdateState
    data class Downloading(val release: AppRelease, val progress: Float) : UpdateState
    /** Handed to the system installer, which shows its own confirm screen. */
    data class Installing(val release: AppRelease) : UpdateState
    /**
     * This app may not install packages yet: Android 8+ asks the person to allow
     * it per app ("install unknown apps"), in the TV's settings.
     */
    data class NeedsPermission(val release: AppRelease) : UpdateState
    data class Failed(val release: AppRelease?, val reason: Reason) : UpdateState

    enum class Reason { CheckFailed, Network, Corrupt, InstallFailed }
}

/**
 * Keeps a sideloaded app up to date. Nothing else will: there is no store.
 *
 *  1. check() asks the viewer what it serves (/api/tv/v1/app) and offers it if
 *     [UpdatePolicy] agrees — once per launch automatically, or on demand.
 *  2. install() downloads it, verifying SHA-256 and size ([ApkDownloader]), and
 *     hands it to the system through a PackageInstaller session. Android shows
 *     its own confirm screen, and it enforces that the update is signed with the
 *     same key as the installed app — the guarantee everything else sits behind.
 *  3. The session's outcome comes back to [InstallStatusReceiver]. On success the
 *     system kills this app to replace it, and does NOT start the new version —
 *     nor may the new version start itself: Android 10+ blocks it as a background
 *     activity launch, both from a MY_PACKAGE_REPLACED receiver and from an
 *     activity PendingIntent as the session's status (both tried on the
 *     emulator). So the UI says beforehand that the app will close.
 *
 * It never interrupts playback: the UI offers updates on Home and in Settings.
 */
class Updater(
    context: Context,
    private val api: PhimnetApi,
    private val downloader: ApkDownloader,
    private val scope: CoroutineScope,
    private val installedVersionCode: Long,
    private val server: () -> HttpUrl,
) {
    private val context = context.applicationContext
    private val dir = File(this.context.cacheDir, "updates")

    private val _state = MutableStateFlow<UpdateState>(UpdateState.Idle)
    val state: StateFlow<UpdateState> = _state.asStateFlow()

    /** "Later" on the Home banner — until the next launch, not forever. */
    private val _dismissed = MutableStateFlow(false)
    val dismissed: StateFlow<Boolean> = _dismissed.asStateFlow()

    private var job: Job? = null

    init {
        // An APK left from an earlier attempt is useless once it is installed or
        // superseded; never let them pile up in the cache.
        dir.listFiles()?.forEach { it.delete() }
    }

    /**
     * Asks the viewer for its current release. An automatic check runs once per
     * launch and stays silent if it fails — a TV offline for a moment has no
     * reason to hear about it. A manual one (Settings) reports every outcome.
     */
    fun check(manual: Boolean = false) {
        if (job?.isActive == true) return
        if (!manual && _state.value != UpdateState.Idle) return
        job = scope.launch {
            _state.value = UpdateState.Checking
            _state.value = try {
                UpdatePolicy.offer(api.appRelease(), installedVersionCode, server())
                    ?.let { UpdateState.Available(it) }
                    ?: UpdateState.UpToDate
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                Log.w(TAG, "update check failed", e)
                if (manual) UpdateState.Failed(null, UpdateState.Reason.CheckFailed) else UpdateState.Idle
            }
        }
    }

    fun dismiss() {
        _dismissed.value = true
    }

    /** Downloads, verifies and installs the offered release. */
    fun install() {
        val release = offeredRelease() ?: return
        if (job?.isActive == true) return
        job = scope.launch {
            if (!canInstall()) {
                _state.value = UpdateState.NeedsPermission(release)
                return@launch
            }
            _state.value = UpdateState.Downloading(release, 0f)
            val apk = try {
                downloader.download(release, File(dir, "phimnet-tv-${release.versionCode}.apk")) { p ->
                    _state.value = UpdateState.Downloading(release, p.coerceIn(0f, 1f))
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: IntegrityException) {
                Log.w(TAG, "update rejected", e)
                _state.value = UpdateState.Failed(release, UpdateState.Reason.Corrupt)
                return@launch
            } catch (e: Exception) {
                Log.w(TAG, "update download failed", e)
                _state.value = UpdateState.Failed(release, UpdateState.Reason.Network)
                return@launch
            }
            _state.value = UpdateState.Installing(release)
            try {
                withContext(Dispatchers.IO) { commit(apk) }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                Log.w(TAG, "update install failed", e)
                _state.value = UpdateState.Failed(release, UpdateState.Reason.InstallFailed)
            }
        }
    }

    /**
     * Opens the TV's per-app "install unknown apps" screen for this app. Returns
     * false when the TV has no such screen — some builds hide it — so the UI can
     * point at the Downloader route instead.
     */
    fun openInstallPermissionSettings(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return false
        val intent = Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, "package:${context.packageName}".toUri())
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        return try {
            context.startActivity(intent)
            true
        } catch (_: ActivityNotFoundException) {
            false
        }
    }

    /**
     * Back from the TV's settings (the activity resumed): if the permission was
     * granted there, carry on with the install instead of making the person find
     * and press Continue.
     */
    fun onAppResumed() {
        if (_state.value is UpdateState.NeedsPermission && canInstall()) install()
    }

    // --- installer callbacks (InstallStatusReceiver) -------------------------------

    internal fun onInstallCancelled() {
        // Backed out of the system's confirm screen: offer it again, no error.
        offeredRelease()?.let { _state.value = UpdateState.Available(it) }
    }

    internal fun onInstallFailed(status: Int, message: String?) {
        Log.w(TAG, "installer status $status: $message")
        _state.value = UpdateState.Failed(offeredRelease(), UpdateState.Reason.InstallFailed)
    }

    // --- internals ----------------------------------------------------------------

    private fun offeredRelease(): AppRelease? = when (val s = _state.value) {
        is UpdateState.Available -> s.release
        is UpdateState.Downloading -> s.release
        is UpdateState.Installing -> s.release
        is UpdateState.NeedsPermission -> s.release
        is UpdateState.Failed -> s.release
        else -> null
    }

    private fun canInstall(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.O || context.packageManager.canRequestPackageInstalls()

    /**
     * Streams the verified APK into a PackageInstaller session and commits it. The
     * session takes a copy, so no FileProvider or shared file is involved.
     */
    private fun commit(apk: File) {
        val installer = context.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(context.packageName)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) setInstallReason(PackageManager.INSTALL_REASON_USER)
        }
        val sessionId = installer.createSession(params)
        installer.openSession(sessionId).use { session ->
            session.openWrite("phimnet-tv.apk", 0, apk.length()).use { out ->
                apk.inputStream().use { it.copyTo(out) }
                session.fsync(out)
            }
            // Explicit intent to our own receiver. MUTABLE is required: the
            // installer adds the status extras to it.
            val flags = PendingIntent.FLAG_UPDATE_CURRENT or
                (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) PendingIntent.FLAG_MUTABLE else 0)
            val status = PendingIntent.getBroadcast(
                context,
                sessionId,
                Intent(context, InstallStatusReceiver::class.java),
                flags,
            )
            session.commit(status.intentSender)
        }
    }

    private companion object {
        const val TAG = "Updater"
    }
}
