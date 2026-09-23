package online.phimnet.tv.api

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Wire types for the viewer's TV API (viewer/tvapi.go) and the two pre-existing
// endpoints this app reuses unchanged (prepare, from viewer/server.go, and the
// streamer's stats).
//
// Field names follow the server byte for byte. Everything the server may omit
// has a default, and WireJson ignores unknown keys, because this APK cannot be
// updated in step with the server: a field the server adds next year must not
// crash the copy installed today.

@Serializable
data class Card(
    val id: Long,
    val type: String,
    val title: String,
    @SerialName("original_title") val originalTitle: String = "",
    val year: String = "",
    val poster: String = "",
    @SerialName("vote_average") val voteAverage: Double = 0.0,
    @SerialName("has_subtitle") val hasSubtitle: Boolean = false,
)

@Serializable
data class Row(
    val key: String,
    val label: String,
    val ranked: Boolean = false,
    val titles: List<Card> = emptyList(),
)

@Serializable
data class Genre(val id: Int, val name: String)

@Serializable
data class Episode(
    val id: Long,
    @SerialName("episode_number") val number: Int,
    val name: String = "",
    val overview: String = "",
    @SerialName("air_date") val airDate: String = "",
    val runtime: Int? = null,
    val still: String = "",
)

@Serializable
data class Season(
    val id: Long,
    @SerialName("season_number") val number: Int,
    val name: String = "",
    val overview: String = "",
    @SerialName("air_date") val airDate: String = "",
    val episodes: List<Episode> = emptyList(),
)

@Serializable
data class Title(
    val id: Long,
    val type: String,
    val title: String,
    @SerialName("original_title") val originalTitle: String = "",
    val overview: String = "",
    @SerialName("air_date") val airDate: String = "",
    val runtime: Int? = null,
    val poster: String = "",
    val backdrop: String = "",
    @SerialName("vote_average") val voteAverage: Double = 0.0,
    val genres: List<Genre> = emptyList(),
    val seasons: List<Season> = emptyList(),
) {
    val isSeries: Boolean get() = type == "tv"
    val year: String get() = airDate.take(4)
}

@Serializable
data class Home(
    val hero: List<Title> = emptyList(),
    val rows: List<Row> = emptyList(),
)

@Serializable
data class TitlePage(
    val page: Int = 1,
    @SerialName("page_size") val pageSize: Int = 0,
    val total: Int = 0,
    val titles: List<Card> = emptyList(),
)

@Serializable
internal data class Genres(val genres: List<Genre> = emptyList())

/**
 * One playable source, exactly as the web watch page receives it (watchVideo in
 * viewer/server.go). [available] and [lock] are computed by the server's
 * resolutionLock; the app never decides either. They are advisory — the prepare
 * endpoint re-checks everything — which is also why the app has no tier logic to
 * get wrong.
 */
@Serializable
data class Video(
    val id: Long,
    val name: String = "",
    val resolution: String,
    @SerialName("file_size") val fileSize: Long = 0,
    val available: Boolean,
    val lock: String = "",
)

/** Why a source cannot be played by this visitor. Mirrors the server's lock constants. */
enum class Lock(val wire: String) {
    None(""),
    /** Sign in and it plays (the 1080p registration nudge, or step one of paying). */
    Member("member"),
    /** Signed in, but this quality is the paid tier. */
    Upgrade("upgrade"),
    /** Nobody can play it yet — billing is not configured on this server. */
    Paid("paid");

    companion object {
        fun of(wire: String): Lock = entries.firstOrNull { it.wire == wire } ?: None
    }
}

val Video.lockKind: Lock get() = Lock.of(lock)

@Serializable
data class Subtitle(
    val id: Long,
    val language: String = "",
    val name: String = "",
    val provider: String = "",
    /** "srt" or "vtt" — what /api/subtitles/{id}/file will serve for this row. */
    val format: String = "vtt",
)

@Serializable
data class Watch(
    val heading: String,
    val sub: String = "",
    @SerialName("owner_kind") val ownerKind: String,
    @SerialName("owner_id") val ownerId: Long,
    @SerialName("title_id") val titleId: Long,
    val videos: List<Video> = emptyList(),
    val subtitles: List<Subtitle> = emptyList(),
    @SerialName("upgrade_url") val upgradeUrl: String = "",
    @SerialName("season_number") val seasonNumber: Int = 0,
    val episodes: List<Episode> = emptyList(),
)

@Serializable
data class Me(
    @SerialName("signed_in") val signedIn: Boolean = false,
    val name: String = "",
    val email: String = "",
    val avatar: String = "",
    val entitled: Boolean = false,
)

/**
 * The response of POST /api/sources/{id}/prepare. camelCase because this is the
 * web watch page's existing endpoint, reused as-is rather than duplicated.
 */
@Serializable
data class Prepared(
    val infoHash: String,
    val fileIndex: Int,
    val streamerPublicURL: String,
)

/**
 * The streamer's GET …/stats. Only the fields the app reads are declared.
 *
 * downloadSpeed / uploadSpeed are deliberately absent: the streamer derives them
 * from ONE sample slot per infohash shared by every caller, so a television and
 * a browser watching the same torrent corrupt each other's readings. Byte counts
 * and peer gauges are read live and are safe.
 */
@Serializable
data class Stats(
    val totalBytes: Long = 0,
    val bytesCompleted: Long = 0,
    val totalPeers: Int = 0,
    val activePeers: Int = 0,
    val connectedSeeders: Int = 0,
)

@Serializable
data class DeviceCode(
    @SerialName("device_code") val deviceCode: String,
    @SerialName("user_code") val userCode: String,
    @SerialName("verification_uri") val verificationUri: String,
    @SerialName("verification_uri_complete") val verificationUriComplete: String = "",
    @SerialName("expires_in") val expiresIn: Int,
    val interval: Int = 5,
)

@Serializable
data class DeviceUser(val name: String = "", val email: String = "")

@Serializable
data class DeviceToken(
    @SerialName("access_token") val accessToken: String,
    @SerialName("token_type") val tokenType: String = "Bearer",
    val user: DeviceUser = DeviceUser(),
)

@Serializable
internal data class ErrorBody(val error: String = "")

@Serializable
internal data class HeartbeatBody(val infoHash: String, val sessionID: String)

@Serializable
internal data class LeaveBody(val sessionID: String)

@Serializable
internal data class DeviceCodeBody(@SerialName("device_name") val deviceName: String)

@Serializable
internal data class DeviceTokenBody(@SerialName("device_code") val deviceCode: String)

/**
 * GET /api/tv/v1/app — the release the viewer currently serves (viewer/tvapk.go),
 * which the installed app compares with its own versionCode. The app is
 * sideloaded, so this is the only way it ever learns an update exists.
 */
@Serializable
data class AppRelease(
    @SerialName("version_code") val versionCode: Long,
    @SerialName("version_name") val versionName: String,
    val sha256: String,
    val size: Long,
    @SerialName("download_url") val downloadUrl: String,
)
