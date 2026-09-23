package online.phimnet.tv.auth

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.withTimeoutOrNull
import online.phimnet.tv.api.DeviceCode
import online.phimnet.tv.api.DeviceToken
import online.phimnet.tv.api.TokenPoll
import kotlin.time.Duration.Companion.seconds

/** The two viewer calls pairing needs; PhimnetApi implements them. */
interface PairingApi {
    suspend fun deviceCode(deviceName: String): DeviceCode
    suspend fun deviceToken(deviceCode: String): TokenPoll
}

sealed interface PairingState {
    data object Requesting : PairingState

    /** Show [code] on screen — the short code, and the QR for the phone. */
    data class Showing(val code: DeviceCode) : PairingState

    data class Paired(val token: DeviceToken) : PairingState

    /**
     * Nobody has approved a code for [MAX_CODES] codes running (an hour, at the
     * viewer's 15-minute expiry). Stop: a television left on this screen would
     * otherwise poll the server every few seconds for as long as it stays on.
     * The screen offers a button to start again.
     */
    data object Idle : PairingState
}

/** Codes to issue before giving up and going [PairingState.Idle]. */
const val MAX_CODES = 4

/**
 * The television's side of device-code pairing (RFC 8628, as the viewer
 * implements it in device.go).
 *
 *  1. Ask for a code and show it.
 *  2. Poll for the token every `interval` seconds — never faster; a `slow_down`
 *     answer adds five seconds, as §3.5 requires.
 *  3. When the code expires (or the server says it is dead), fetch a fresh one
 *     and show that instead. Someone who wandered off to find their phone comes
 *     back to a working code rather than an error they have to dismiss.
 *  4. After [MAX_CODES] unused codes, stop ([PairingState.Idle]). Found by leaving
 *     the pairing screen up for three hours: it faithfully polled 2,217 times.
 *
 * A failure to obtain a code at all is thrown to the collector, which offers a
 * retry. A failed POLL is not: one dropped request on a flaky Wi-Fi link must not
 * cost the person the code they are halfway through typing.
 */
fun pairingFlow(api: PairingApi, deviceName: String, maxCodes: Int = MAX_CODES): Flow<PairingState> = flow {
    repeat(maxCodes) {
        emit(PairingState.Requesting)
        val code = api.deviceCode(deviceName)
        emit(PairingState.Showing(code))

        val token = withTimeoutOrNull(code.expiresIn.seconds) { pollForToken(api, code) }
        if (token != null) {
            emit(PairingState.Paired(token))
            return@flow
        }
        // Expired or dead: loop round and show a fresh code.
    }
    emit(PairingState.Idle)
}

/** Polls until the token is granted, or returns null once the code is dead. */
private suspend fun pollForToken(api: PairingApi, code: DeviceCode): DeviceToken? {
    var interval = code.interval.coerceAtLeast(1).seconds
    while (true) {
        delay(interval)
        val poll = try {
            api.deviceToken(code.deviceCode)
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            null // transient: keep the code on screen and try again next interval
        }
        when (poll) {
            is TokenPoll.Granted -> return poll.token
            TokenPoll.Expired -> return null
            TokenPoll.SlowDown -> interval += 5.seconds
            TokenPoll.Pending, null -> Unit
        }
    }
}
