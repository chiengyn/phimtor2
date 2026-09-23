package online.phimnet.tv.ui.common

import android.util.Log
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.CancellationException
import online.phimnet.tv.LocalGraph
import online.phimnet.tv.R
import online.phimnet.tv.api.ApiException
import java.io.IOException

/** A screen's data: loading, loaded, or failed with something to show. */
sealed interface Load<out T> {
    data object Loading : Load<Nothing>
    data class Ready<T>(val value: T) : Load<T>
    data class Failed(val error: Throwable) : Load<Nothing>
}

class Loaded<T>(val state: Load<T>, val retry: () -> Unit)

/**
 * Runs [block] and exposes its state, re-running it when any of [keys] change or
 * when retry() is called. The small thing every browse screen needs; the player,
 * whose lifecycle is far richer, has its own ViewModel instead.
 */
@Composable
fun <T> rememberLoad(vararg keys: Any?, block: suspend () -> T): Loaded<T> {
    var attempt by remember { mutableIntStateOf(0) }
    val state by produceState<Load<T>>(Load.Loading, *keys, attempt) {
        value = Load.Loading
        value = try {
            Load.Ready(block())
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // The screen shows a friendly sentence; logcat gets the real cause.
            Log.w("Load", "load failed", e)
            Load.Failed(e)
        }
    }
    return Loaded(state) { attempt++ }
}

/**
 * The text to show for a failure. The viewer's own message wins when there is
 * one, because for the statuses that matter (the gated 401/402/403) it has
 * already been translated into the person's language server-side.
 */
@Composable
fun errorText(error: Throwable): String = when {
    error is ApiException && error.status in 400..499 && !error.message.isNullOrBlank() -> error.message!!
    error is ApiException -> stringResource(R.string.error_server)
    // Only a genuine I/O failure is "can't reach the server". Anything else — a
    // payload that would not decode, a bug — must not send someone off to check
    // a Wi-Fi connection that is fine.
    error is IOException -> stringResource(R.string.error_network)
    else -> stringResource(R.string.error_server)
}

/**
 * Everything a viewer response depends on: which server, as whom, in which
 * language. Screens key their loads on it, so switching the language or server
 * in Settings — or pairing, which changes what the watch endpoints unlock —
 * refetches instead of showing stale answers.
 */
data class SessionKey(val server: String, val token: String?, val locale: String)

@Composable
fun rememberSessionKey(): SessionKey {
    val graph = LocalGraph.current
    val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
    return SessionKey(
        server = prefs?.serverUrl.orEmpty(),
        token = prefs?.token,
        locale = graph.contentLocale(prefs),
    )
}
