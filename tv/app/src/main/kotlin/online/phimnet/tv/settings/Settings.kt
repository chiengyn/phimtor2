package online.phimnet.tv.settings

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn

private val Context.dataStore: DataStore<Preferences> by preferencesDataStore(name = "settings")

/** Everything the app remembers between launches. */
data class Prefs(
    val serverUrl: String,
    /** The paired television's bearer token, or null when not signed in. */
    val token: String?,
    /** Who the token belongs to, for the account screen. */
    val userName: String?,
    /** A language chosen in Settings, or null to follow the device. */
    val localeOverride: String?,
    /**
     * The quality last chosen BY HAND. Same key name as the web page's
     * localStorage (`phimnet.quality`) because it is the same idea: a
     * convenience that auto-play honours when it can, never an entitlement.
     */
    val quality: String?,
)

class Settings(context: Context, scope: CoroutineScope, private val defaultServer: String) {

    private val store = context.applicationContext.dataStore

    /** Null until the first read completes; the UI waits for it (see MainActivity). */
    val prefs: StateFlow<Prefs?> = store.data
        .map { it.toPrefs() }
        .stateIn(scope, SharingStarted.Eagerly, null)

    suspend fun current(): Prefs = prefs.value ?: store.data.first().toPrefs()

    /**
     * Points the app at another viewer. This ALSO signs out: a token is minted by
     * one server's SESSION_SECRET and names a pairing row in that server's
     * database, so it means nothing anywhere else — and sending it to a different
     * host would hand a live credential to a server that never issued it.
     */
    suspend fun setServerUrl(url: String) {
        store.edit {
            if (it[SERVER] != url) {
                it.remove(TOKEN)
                it.remove(USER)
            }
            it[SERVER] = url
        }
    }

    suspend fun signIn(token: String, userName: String) {
        store.edit {
            it[TOKEN] = token
            it[USER] = userName
        }
    }

    suspend fun signOut() {
        store.edit {
            it.remove(TOKEN)
            it.remove(USER)
        }
    }

    suspend fun setLocaleOverride(locale: String?) {
        store.edit { if (locale == null) it.remove(LOCALE) else it[LOCALE] = locale }
    }

    suspend fun setQuality(resolution: String) {
        store.edit { it[QUALITY] = resolution }
    }

    // --- resume positions ------------------------------------------------------
    //
    // Kept on the device, as the web page keeps them in localStorage. Keyed by
    // what is being watched ("movie:9", "episode:42"), not by source, so switching
    // quality mid-film does not lose the place.

    suspend fun resumePosition(key: String): Long = store.data.first()[resumeKey(key)] ?: 0L

    suspend fun saveResumePosition(key: String, positionMs: Long) {
        store.edit { it[resumeKey(key)] = positionMs }
    }

    suspend fun clearResumePosition(key: String) {
        store.edit { it.remove(resumeKey(key)) }
    }

    private fun Preferences.toPrefs() = Prefs(
        serverUrl = this[SERVER] ?: defaultServer,
        token = this[TOKEN],
        userName = this[USER],
        localeOverride = this[LOCALE],
        quality = this[QUALITY],
    )

    private companion object {
        val SERVER = stringPreferencesKey("server_url")
        val TOKEN = stringPreferencesKey("device_token")
        val USER = stringPreferencesKey("device_user")
        val LOCALE = stringPreferencesKey("locale")
        val QUALITY = stringPreferencesKey("phimnet.quality")
        fun resumeKey(key: String) = longPreferencesKey("resume.$key")
    }
}
