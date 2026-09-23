package online.phimnet.tv.api

import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.json.Json
import okhttp3.Call
import okhttp3.Callback
import okhttp3.Response
import java.io.IOException
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/**
 * The one JSON configuration every wire type is decoded with.
 *
 * ignoreUnknownKeys is the load-bearing setting: the server may add a field at
 * any time and an installed APK cannot be updated in step, so an unexpected key
 * must be ignored, never fatal. coerceInputValues likewise turns a null where a
 * default exists into that default rather than an exception.
 */
val WireJson: Json = Json {
    ignoreUnknownKeys = true
    coerceInputValues = true
    explicitNulls = false
}

/**
 * A non-2xx answer from the viewer. [message] is the server's own `error` string,
 * which for the gated statuses (401 / 402 / 403 from prepare) is already
 * localised and fit to show as-is.
 */
class ApiException(val status: Int, message: String) : IOException(message)

/** Suspends on an OkHttp call, cancelling the call if the coroutine is cancelled. */
suspend fun Call.await(): Response = suspendCancellableCoroutine { cont ->
    cont.invokeOnCancellation { cancel() }
    enqueue(object : Callback {
        override fun onFailure(call: Call, e: IOException) {
            if (cont.isActive) cont.resumeWithException(e)
        }

        override fun onResponse(call: Call, response: Response) {
            cont.resume(response) { _, value, _ -> value.close() }
        }
    })
}

/** Reads a non-2xx response into an [ApiException], preferring the server's own message. */
internal fun Response.toApiException(): ApiException {
    val raw = body.string()
    val message = runCatching { WireJson.decodeFromString<ErrorBody>(raw).error }
        .getOrNull()
        ?.takeIf { it.isNotBlank() }
        ?: "HTTP $code"
    return ApiException(code, message)
}
