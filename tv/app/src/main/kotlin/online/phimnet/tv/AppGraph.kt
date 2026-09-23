package online.phimnet.tv

import android.app.Application
import android.os.Build
import androidx.compose.runtime.staticCompositionLocalOf
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import okhttp3.OkHttpClient
import online.phimnet.tv.api.ApiSession
import online.phimnet.tv.api.PhimnetApi
import online.phimnet.tv.api.StreamerClient
import online.phimnet.tv.api.baseUrlOf
import online.phimnet.tv.settings.Prefs
import online.phimnet.tv.settings.Settings
import online.phimnet.tv.settings.SiteLocale
import online.phimnet.tv.update.ApkDownloader
import online.phimnet.tv.update.Updater
import java.util.Locale
import java.util.concurrent.TimeUnit

/**
 * The app's few long-lived objects, built once. Hand-wired rather than via a DI
 * framework — there are six of them, and this repo prefers a small explicit
 * client over a dependency (the Go services hand-write their HTTP clients too).
 */
class AppGraph(app: Application) {

    /** Outlives every screen: used for work that must finish as a screen is torn down. */
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)

    val settings = Settings(app, appScope, BuildConfig.DEFAULT_SERVER_URL)

    val userAgent = "phimnet-tv/${BuildConfig.VERSION_NAME} (Android ${Build.VERSION.RELEASE}; ${Build.MODEL})"

    /** For the viewer's JSON API. */
    val http: OkHttpClient = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .build()

    /**
     * For the streamer: ExoPlayer's reads and the stats poll. Shares the viewer
     * client's connection pool, but a torrent stream can go quiet for a long time
     * while the swarm fetches the next pieces, so it waits longer before calling a
     * read dead. It never carries the viewer's token (see PhimnetApi).
     */
    val streamHttp: OkHttpClient = http.newBuilder()
        .readTimeout(90, TimeUnit.SECONDS)
        .build()

    val api = PhimnetApi(http, ::session, userAgent)

    val streamer = StreamerClient(streamHttp, userAgent)

    /** Keeps the sideloaded app up to date from the viewer (update/Updater.kt). */
    val updater = Updater(
        context = app,
        api = api,
        downloader = ApkDownloader(http, userAgent),
        scope = appScope,
        installedVersionCode = BuildConfig.VERSION_CODE.toLong(),
        server = { session().baseUrl },
    )

    private fun session(): ApiSession {
        val prefs = settings.prefs.value
            ?: Prefs(BuildConfig.DEFAULT_SERVER_URL, null, null, null, null)
        return ApiSession(baseUrlOf(prefs.serverUrl), prefs.token, contentLocale(prefs))
    }

    /**
     * The language catalog content is requested in: the one picked in Settings,
     * else the device's (which, on Android 13+, includes a per-app language set in
     * system settings).
     */
    fun contentLocale(prefs: Prefs?): String =
        prefs?.localeOverride ?: SiteLocale.fromAndroid(Locale.getDefault())
}

val LocalGraph = staticCompositionLocalOf<AppGraph> { error("AppGraph not provided") }
