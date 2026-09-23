package online.phimnet.tv.auth

import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.test.currentTime
import kotlinx.coroutines.test.runTest
import online.phimnet.tv.api.DeviceCode
import online.phimnet.tv.api.DeviceToken
import online.phimnet.tv.api.DeviceUser
import online.phimnet.tv.api.TokenPoll
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException

class PairingFlowTest {

    /** A scripted viewer: codes are handed out in order, polls answered from a queue. */
    private class FakeViewer(private val polls: ArrayDeque<() -> TokenPoll>, private val expiresIn: Int = 900) : PairingApi {
        var codesIssued = 0
        val pollTimes = mutableListOf<Long>()
        lateinit var clock: () -> Long

        override suspend fun deviceCode(deviceName: String): DeviceCode {
            codesIssued++
            return DeviceCode(
                deviceCode = "device-$codesIssued",
                userCode = "ACDE-FGH$codesIssued",
                verificationUri = "https://viewer/vi/link",
                verificationUriComplete = "https://viewer/vi/link?code=ACDE-FGH$codesIssued",
                expiresIn = expiresIn,
                interval = 5,
            )
        }

        override suspend fun deviceToken(deviceCode: String): TokenPoll {
            pollTimes += clock()
            return (polls.removeFirstOrNull() ?: { TokenPoll.Pending })()
        }
    }

    private val granted = TokenPoll.Granted(DeviceToken("token-xyz", user = DeviceUser(name = "Chien")))

    @Test
    fun showsTheCodeThenHandsOverTheTokenOnceApproved() = runTest {
        val viewer = FakeViewer(ArrayDeque(listOf({ TokenPoll.Pending }, { TokenPoll.Pending }, { granted })))
        viewer.clock = { currentTime }

        val states = pairingFlow(viewer, "Living room").toList()

        assertEquals(PairingState.Requesting, states[0])
        assertEquals("ACDE-FGH1", (states[1] as PairingState.Showing).code.userCode)
        assertEquals("token-xyz", (states.last() as PairingState.Paired).token.accessToken)
        // Never polls faster than the interval the server asked for.
        assertEquals(listOf(5_000L, 10_000L, 15_000L), viewer.pollTimes)
    }

    // RFC 8628 §3.5: slow_down means add five seconds to the interval, for good.
    @Test
    fun slowDownWidensThePollInterval() = runTest {
        val viewer = FakeViewer(ArrayDeque(listOf({ TokenPoll.SlowDown }, { TokenPoll.Pending }, { granted })))
        viewer.clock = { currentTime }

        pairingFlow(viewer, "TV").toList()

        assertEquals(listOf(5_000L, 15_000L, 25_000L), viewer.pollTimes)
    }

    // Someone who wandered off to find their phone comes back to a working code,
    // not an error they have to dismiss.
    @Test
    fun anExpiredCodeIsReplacedWithAFreshOne() = runTest {
        val viewer = FakeViewer(ArrayDeque(listOf({ TokenPoll.Expired }, { granted })))
        viewer.clock = { currentTime }

        val states = pairingFlow(viewer, "TV").toList()

        val shown = states.filterIsInstance<PairingState.Showing>().map { it.code.userCode }
        assertEquals(listOf("ACDE-FGH1", "ACDE-FGH2"), shown)
        assertTrue(states.last() is PairingState.Paired)
    }

    @Test
    fun aCodeThatOutlivesItsExpiryIsReplacedEvenWithoutTheServerSayingSo() = runTest {
        // 12s of life and a 5s interval: two pending polls, then it must be replaced.
        val viewer = FakeViewer(ArrayDeque(listOf({ TokenPoll.Pending }, { TokenPoll.Pending }, { granted })), expiresIn = 12)
        viewer.clock = { currentTime }

        val states = pairingFlow(viewer, "TV").toList()

        assertEquals(2, viewer.codesIssued)
        assertTrue(states.last() is PairingState.Paired)
    }

    // One dropped request on a flaky link must not cost the person the code they
    // are halfway through typing.
    @Test
    fun aFailedPollKeepsTheSameCodeOnScreen() = runTest {
        val viewer = FakeViewer(ArrayDeque(listOf({ throw IOException("wifi blip") }, { granted })))
        viewer.clock = { currentTime }

        val states = pairingFlow(viewer, "TV").toList()

        assertEquals(1, viewer.codesIssued)
        assertTrue(states.last() is PairingState.Paired)
    }

    // Found by leaving the pairing screen up for three hours during testing: it
    // polled 2,217 times. An unattended TV must stop after MAX_CODES codes.
    @Test
    fun anUnattendedTelevisionStopsPollingAfterAnHour() = runTest {
        val viewer = FakeViewer(ArrayDeque(), expiresIn = 900) // nobody ever approves
        viewer.clock = { currentTime }

        val states = pairingFlow(viewer, "TV").toList()

        assertEquals(MAX_CODES, viewer.codesIssued)
        assertEquals(PairingState.Idle, states.last())
        assertEquals("gives up after MAX_CODES × 15 minutes", MAX_CODES * 900_000L, currentTime)
    }
}
