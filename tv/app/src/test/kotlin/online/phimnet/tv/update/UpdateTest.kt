package online.phimnet.tv.update

import kotlinx.coroutines.test.runTest
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import online.phimnet.tv.api.AppRelease
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.security.MessageDigest

private fun sha256(bytes: ByteArray) =
    MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it) }

class UpdatePolicyTest {

    private val server = "https://phimnet.online".toHttpUrl()

    private fun release(
        code: Long = 1001,
        url: String = "https://phimnet.online/download/phimnet-tv.apk",
        size: Long = 3_000_000,
        sha: String = "a".repeat(64),
    ) = AppRelease(versionCode = code, versionName = "0.1.1", sha256 = sha, size = size, downloadUrl = url)

    @Test
    fun offersAStrictlyNewerRelease() {
        assertEquals(1001L, UpdatePolicy.offer(release(code = 1001), 1000, server)?.versionCode)
    }

    // Android refuses an equal or lower versionCode (INSTALL_FAILED_VERSION_DOWNGRADE,
    // confirmed on an emulator), so offering one would only ever fail.
    @Test
    fun neverOffersTheSameOrAnOlderVersion() {
        assertNull(UpdatePolicy.offer(release(code = 1000), 1000, server))
        assertNull(UpdatePolicy.offer(release(code = 999), 1000, server))
    }

    @Test
    fun noReleaseIsNoOffer() {
        assertNull(UpdatePolicy.offer(null, 1000, server))
    }

    // The manifest is served by the configured viewer, so a download URL anywhere
    // else means something is wrong — never fetch it.
    @Test
    fun onlyDownloadsFromTheConfiguredViewer() {
        assertNull(UpdatePolicy.offer(release(url = "https://evil.example/phimnet-tv.apk"), 1000, server))
        assertNull(UpdatePolicy.offer(release(url = "http://phimnet.online/download/phimnet-tv.apk"), 1000, server))
        assertNull(UpdatePolicy.offer(release(url = "https://phimnet.online:8443/x.apk"), 1000, server))
        assertTrue(UpdatePolicy.sameOrigin("https://phimnet.online/tv", server))
        assertFalse(UpdatePolicy.sameOrigin("not a url", server))
    }

    @Test
    fun rejectsAManifestItCannotVerifyAgainst() {
        assertNull(UpdatePolicy.offer(release(sha = "not-a-hash"), 1000, server))
        assertNull(UpdatePolicy.offer(release(size = 0), 1000, server))
        assertNull(UpdatePolicy.offer(release(size = UpdatePolicy.MAX_APK_BYTES + 1), 1000, server))
    }
}

class ApkDownloaderTest {

    @get:Rule val tmp = TemporaryFolder()
    private val server = MockWebServer()
    private val apk = ByteArray(200_000) { (it * 31 % 251).toByte() }

    @Before fun setUp() = server.start()

    @After fun tearDown() = server.close()

    private fun releaseFor(bytes: ByteArray, sha: String = sha256(bytes), size: Long = bytes.size.toLong()) =
        AppRelease(1001, "0.1.1", sha, size, server.url("/download/phimnet-tv.apk").toString())

    private fun serve(bytes: ByteArray) =
        server.enqueue(MockResponse.Builder().body(okio.Buffer().write(bytes)).build())

    private val downloader = ApkDownloader(OkHttpClient(), "test")

    @Test
    fun downloadsAndVerifiesTheApk() = runTest {
        serve(apk)
        val progress = mutableListOf<Float>()
        val file = downloader.download(releaseFor(apk), tmp.root.resolve("u/app.apk")) { progress += it }

        assertArrayEquals(apk, file.readBytes())
        assertEquals(1f, progress.last(), 0.0001f)
        assertFalse("no partial file left", tmp.root.resolve("u/app.apk.part").exists())
    }

    // A tampered or damaged file must never reach the installer — and must not
    // be left lying in the cache either.
    @Test
    fun rejectsBytesThatDoNotMatchTheHash() = runTest {
        serve(apk)
        val dest = tmp.root.resolve("app.apk")
        try {
            downloader.download(releaseFor(apk, sha = "0".repeat(64)), dest)
            fail("expected IntegrityException")
        } catch (_: IntegrityException) {
        }
        assertFalse(dest.exists())
        assertFalse(tmp.root.resolve("app.apk.part").exists())
    }

    @Test
    fun rejectsATruncatedDownload() = runTest {
        serve(apk.copyOf(apk.size - 10))
        try {
            downloader.download(releaseFor(apk), tmp.root.resolve("app.apk"))
            fail("expected IntegrityException")
        } catch (_: IntegrityException) {
        }
    }

    // Stops the moment the body overruns what the manifest promised, instead of
    // reading an unbounded stream to disk first.
    @Test
    fun stopsReadingOnceTheBodyExceedsTheManifest() = runTest {
        serve(apk)
        try {
            downloader.download(releaseFor(apk, size = 1000), tmp.root.resolve("app.apk"))
            fail("expected IntegrityException")
        } catch (e: IntegrityException) {
            assertTrue(e.message!!.contains("more than"))
        }
    }
}
