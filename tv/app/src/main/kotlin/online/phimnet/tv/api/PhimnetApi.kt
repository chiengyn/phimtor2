package online.phimnet.tv.api

import androidx.annotation.Keep
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.KSerializer
import kotlinx.serialization.serializer
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import online.phimnet.tv.auth.PairingApi

/** What every viewer request is made with: which server, as whom, in which language. */
data class ApiSession(
    val baseUrl: HttpUrl,
    val token: String?,
    /** One of the viewer's locales: vi, en, zh-cn, zh-tw, ko, ja. */
    val locale: String,
)

/**
 * @Keep because this enum rides in a navigation route, which serialises it by
 * name — R8 renaming its constants in a release build would break every link to
 * the player while debug builds kept working.
 */
@Keep
enum class WatchKind(val path: String) { Movie("movie"), Episode("episode") }

/** The outcome of one RFC 8628 token poll. */
sealed interface TokenPoll {
    data class Granted(val token: DeviceToken) : TokenPoll
    /** Nobody has approved the code yet; ask again after the interval. */
    data object Pending : TokenPoll
    /** Polling too fast; RFC 8628 says add five seconds to the interval. */
    data object SlowDown : TokenPoll
    /** The code is dead (expired, unknown or revoked) — start over with a new one. */
    data object Expired : TokenPoll
}

/**
 * The client for this app's viewer: the TV API (/api/tv/v1/…) plus the web
 * watch page's own endpoints, which the TV reuses unchanged — prepare, the watch
 * heartbeat and the subtitle file.
 *
 * The bearer token is attached HERE, per request, and deliberately not by an
 * OkHttp interceptor. The same OkHttpClient also feeds ExoPlayer, whose requests
 * go to a streamer — another host — and the viewer's token must never be sent
 * there. The stream endpoint needs no credential anyway.
 */
class PhimnetApi(
    private val http: OkHttpClient,
    private val session: () -> ApiSession,
    private val userAgent: String,
) : PairingApi {
    suspend fun home(): Home = get("api/tv/v1/home")

    suspend fun titles(query: String = "", genre: Int? = null, type: String? = null, page: Int = 1): TitlePage =
        get("api/tv/v1/titles") {
            if (query.isNotBlank()) addQueryParameter("q", query.trim())
            genre?.let { addQueryParameter("genre", it.toString()) }
            type?.let { addQueryParameter("type", it) }
            addQueryParameter("page", page.toString())
        }

    suspend fun genres(): List<Genre> = get<Genres>("api/tv/v1/genres").genres

    suspend fun title(id: Long): Title = get("api/tv/v1/titles/$id")

    suspend fun watch(kind: WatchKind, id: Long): Watch = get("api/tv/v1/watch/${kind.path}/$id")

    suspend fun me(): Me = get("api/tv/v1/me")

    /**
     * The release the viewer serves, or null when it serves none — nothing
     * published yet, or a viewer without TV_APK_DIR (both answer 404). A null is
     * "no update", never an error worth showing.
     */
    suspend fun appRelease(): AppRelease? = try {
        get<AppRelease>("api/tv/v1/app")
    } catch (e: ApiException) {
        if (e.status == 404) null else throw e
    }

    /**
     * Asks the viewer to put this source's torrent on a streamer. This is the
     * same endpoint the web watch page calls, and the only one that ENFORCES the
     * quality tiers: 401 means "sign in", 402 "upgrade", 403 "nobody can yet",
     * each with a localised message the caller can show as-is.
     */
    suspend fun prepare(videoId: Long): Prepared = post("api/sources/$videoId/prepare", body = null)

    suspend fun heartbeat(infoHash: String, sessionId: String) {
        postUnit("api/watch/heartbeat", HeartbeatBody(infoHash, sessionId), HeartbeatBody.serializer())
    }

    suspend fun leave(sessionId: String) {
        postUnit("api/watch/leave", LeaveBody(sessionId), LeaveBody.serializer())
    }

    override suspend fun deviceCode(deviceName: String): DeviceCode =
        post("api/tv/v1/device/code", encode(DeviceCodeBody(deviceName), DeviceCodeBody.serializer()))

    override suspend fun deviceToken(deviceCode: String): TokenPoll {
        val body = encode(DeviceTokenBody(deviceCode), DeviceTokenBody.serializer())
        return try {
            TokenPoll.Granted(post<DeviceToken>("api/tv/v1/device/token", body))
        } catch (e: ApiException) {
            // RFC 8628 §3.5 error codes, which the viewer sends verbatim.
            when {
                e.status != 400 -> throw e
                e.message == "authorization_pending" -> TokenPoll.Pending
                e.message == "slow_down" -> TokenPoll.SlowDown
                else -> TokenPoll.Expired
            }
        }
    }

    /**
     * Revokes THIS television's pairing on the server, so the token stops
     * working at once instead of living out its 180 days after "sign out".
     */
    suspend fun deviceLogout() {
        postUnit<Unit>("api/tv/v1/device/logout", null, null)
    }

    /** The saved-subtitle file. Anonymous on the server, so it carries no token. */
    fun subtitleUrl(id: Long): String = url("api/subtitles/$id/file").toString()

    /**
     * Resolves a server-relative path (the upgrade URL arrives as "/vi/plans?…")
     * against the configured viewer, so it can be rendered as a QR code.
     */
    fun absolute(pathOrUrl: String): String =
        session().baseUrl.resolve(pathOrUrl)?.toString() ?: pathOrUrl

    // --- plumbing ---------------------------------------------------------------

    private fun url(path: String, query: HttpUrl.Builder.() -> Unit = {}): HttpUrl =
        session().baseUrl.newBuilder().addPathSegments(path).apply(query).build()

    private fun request(url: HttpUrl): Request.Builder {
        val s = session()
        return Request.Builder()
            .url(url)
            .header("User-Agent", userAgent)
            // The viewer's existing convention for API locale (handlePrepareSource
            // and the subtitle API read it too), so server-authored text — lock
            // messages, row labels, episode captions — arrives already translated.
            .header("X-Phimnet-Locale", s.locale)
            .apply { s.token?.let { header("Authorization", "Bearer $it") } }
    }

    private suspend inline fun <reified T> get(path: String, noinline query: HttpUrl.Builder.() -> Unit = {}): T =
        execute(request(url(path, query)).get().build(), serializer())

    private suspend inline fun <reified T> post(path: String, body: String?): T =
        execute(request(url(path)).post((body ?: "").toRequestBody(JSON)).build(), serializer())

    private suspend fun <B> postUnit(path: String, body: B?, serializer: KSerializer<B>?) {
        val json = if (body != null && serializer != null) encode(body, serializer) else ""
        val request = request(url(path)).post(json.toRequestBody(JSON)).build()
        withContext(Dispatchers.IO) {
            http.newCall(request).await().use { response ->
                if (!response.isSuccessful) throw response.toApiException()
            }
        }
    }

    /**
     * Main-safe, like every suspend function here: callers are Compose screens on
     * the main thread, and await() resumes on the CALLER's dispatcher — so reading
     * the body there would be a socket read on the UI thread, which Android kills
     * with NetworkOnMainThreadException. The whole exchange runs on IO instead.
     */
    private suspend fun <T> execute(request: Request, serializer: KSerializer<T>): T =
        withContext(Dispatchers.IO) {
            http.newCall(request).await().use { response ->
                if (!response.isSuccessful) throw response.toApiException()
                WireJson.decodeFromString(serializer, response.body.string())
            }
        }

    private fun <B> encode(body: B, serializer: KSerializer<B>): String = WireJson.encodeToString(serializer, body)

    private companion object {
        val JSON = "application/json".toMediaType()
    }
}

/** Parses a base URL the app has already validated (see ServerUrl). */
fun baseUrlOf(value: String): HttpUrl = value.toHttpUrl()
