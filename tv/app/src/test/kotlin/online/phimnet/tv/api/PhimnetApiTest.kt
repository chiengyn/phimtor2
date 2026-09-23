package online.phimnet.tv.api

import kotlinx.coroutines.test.runTest
import mockwebserver3.MockResponse
import mockwebserver3.MockWebServer
import okhttp3.OkHttpClient
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class PhimnetApiTest {

    private val server = MockWebServer()
    private var token: String? = null
    private lateinit var api: PhimnetApi

    @Before
    fun setUp() {
        server.start()
        api = PhimnetApi(
            http = OkHttpClient(),
            session = { ApiSession(server.url("/"), token, "en") },
            userAgent = "phimnet-tv/test",
        )
    }

    @After
    fun tearDown() = server.close()

    private fun respond(code: Int, body: String) =
        server.enqueue(MockResponse.Builder().code(code).body(body).build())

    @Test
    fun sendsTheLocaleAndTheBearerTokenToTheViewer() = runTest {
        token = "v1tv.secret"
        respond(200, """{"signed_in":true,"name":"Chien"}""")

        val me = api.me()

        val request = server.takeRequest()
        assertEquals("/api/tv/v1/me", request.url.encodedPath)
        assertEquals("en", request.headers["X-Phimnet-Locale"])
        assertEquals("Bearer v1tv.secret", request.headers["Authorization"])
        assertEquals("Chien", me.name)
    }

    @Test
    fun anAnonymousRequestCarriesNoAuthorization() = runTest {
        respond(200, """{"signed_in":false}""")
        api.me()
        assertNull(server.takeRequest().headers["Authorization"])
    }

    // An installed APK cannot be updated in step with the server, so a field the
    // server adds later must be ignored, never fatal.
    @Test
    fun fieldsTheServerAddsLaterAreIgnored() = runTest {
        respond(200, """{"page":2,"page_size":60,"total":61,"titles":[
            {"id":9,"type":"movie","title":"Kẻ Trộm Giấc Mơ","year":"2010","brand_new_field":{"x":1}}
        ],"also_new":true}""")

        val page = api.titles(query = "dream", page = 2)

        assertEquals(60, page.pageSize)
        assertEquals("Kẻ Trộm Giấc Mơ", page.titles.single().title)
        val request = server.takeRequest()
        assertEquals("dream", request.url.queryParameter("q"))
        assertEquals("2", request.url.queryParameter("page"))
    }

    // prepare is the ONLY enforcement of the quality tiers. Its gated answers
    // carry a message the server has already localised, fit to show as-is.
    @Test
    fun aGatedPrepareSurfacesTheStatusAndTheServersMessage() = runTest {
        respond(402, """{"error":"Upgrade to watch in 2160p"}""")
        try {
            api.prepare(45)
            fail("expected ApiException")
        } catch (e: ApiException) {
            assertEquals(402, e.status)
            assertEquals("Upgrade to watch in 2160p", e.message)
        }
        val request = server.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/api/sources/45/prepare", request.url.encodedPath)
    }

    @Test
    fun decodesThePrepareAnswerTheWebPageAlsoGets() = runTest {
        respond(200, """{"infoHash":"abc","fileIndex":3,"streamerPublicURL":"https://s1.example"}""")
        assertEquals(Prepared("abc", 3, "https://s1.example"), api.prepare(1))
    }

    @Test
    fun mapsTheRfc8628PollErrors() = runTest {
        respond(400, """{"error":"authorization_pending"}""")
        respond(400, """{"error":"slow_down"}""")
        respond(400, """{"error":"expired_token"}""")
        respond(200, """{"access_token":"t","token_type":"Bearer","expires_in":1,"user":{"name":"N"}}""")

        assertEquals(TokenPoll.Pending, api.deviceToken("d"))
        assertEquals(TokenPoll.SlowDown, api.deviceToken("d"))
        assertEquals(TokenPoll.Expired, api.deviceToken("d"))
        assertEquals("t", (api.deviceToken("d") as TokenPoll.Granted).token.accessToken)

        assertTrue(server.takeRequest().body!!.utf8().contains("\"device_code\":\"d\""))
    }

    // The heartbeat is the web page's existing endpoint and keeps its camelCase body.
    @Test
    fun heartbeatUsesTheWebPagesBodyShape() = runTest {
        respond(204, "")
        api.heartbeat("hash", "session")
        assertEquals("""{"infoHash":"hash","sessionID":"session"}""", server.takeRequest().body!!.utf8())
    }

    @Test
    fun upgradeLinksResolveAgainstTheConfiguredViewer() {
        assertEquals(server.url("/vi/plans?title=9").toString(), api.absolute("/vi/plans?title=9"))
    }
}
