package online.phimnet.tv.settings

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Locale

class ServerUrlTest {

    private fun ok(input: String, cleartext: Boolean = false): String {
        val result = ServerUrl.parse(input, cleartext)
        assertTrue("$input → $result", result is ServerUrl.Result.Ok)
        return ServerUrl.format((result as ServerUrl.Result.Ok).url)
    }

    // Typed with a remote, so it has to be forgiving.
    @Test
    fun aBareHostMeansHttps() {
        assertEquals("https://phimnet.online", ok("phimnet.online"))
        assertEquals("https://phimnet.online", ok("  https://phimnet.online/  "))
    }

    @Test
    fun aSelfHostedPathPrefixIsKept() {
        assertEquals("https://home.example/phim", ok("home.example/phim/"))
    }

    // The bearer token rides on every request, so a release build will not send
    // it in the clear. Debug builds allow http for a local `go run .` viewer.
    @Test
    fun plainHttpIsRefusedUnlessAllowed() {
        assertEquals(ServerUrl.Result.CleartextNotAllowed, ServerUrl.parse("http://192.168.1.5:8082", false))
        assertEquals("http://10.0.2.2:8082", ok("http://10.0.2.2:8082", cleartext = true))
    }

    @Test
    fun garbageIsInvalid() {
        for (input in listOf("", "   ", "https://", "https://host/?q=1", "ftp://host")) {
            assertEquals(input, ServerUrl.Result.Invalid, ServerUrl.parse(input, true))
        }
    }
}

// Mirrors parseLocale in viewer/i18n.go, so the television and the website agree
// on which language a person gets.
class SiteLocaleTest {

    @Test
    fun matchesTheSitesLanguages() {
        assertEquals("vi", SiteLocale.fromAndroid(Locale.forLanguageTag("vi-VN")))
        assertEquals("en", SiteLocale.fromAndroid(Locale.forLanguageTag("en-GB")))
        assertEquals("ko", SiteLocale.fromAndroid(Locale.forLanguageTag("ko-KR")))
        assertEquals("ja", SiteLocale.fromAndroid(Locale.forLanguageTag("ja-JP")))
        assertEquals("th", SiteLocale.fromAndroid(Locale.forLanguageTag("th-TH")))
        assertEquals("es", SiteLocale.fromAndroid(Locale.forLanguageTag("es-MX")))
        assertEquals("ru", SiteLocale.fromAndroid(Locale.forLanguageTag("ru-RU")))
    }

    @Test
    fun mapsIndonesianAndPortugueseOntoTheSitesCodes() {
        assertEquals("id", SiteLocale.fromAndroid(Locale.forLanguageTag("id-ID")))
        // The pre-1989 code older Android reports. A JDK 17+ JVM already rewrites
        // it to "id", so this pins the input, not the "in" branch itself.
        assertEquals("id", SiteLocale.fromAndroid(Locale.forLanguageTag("in-ID")))
        assertEquals("pt-br", SiteLocale.fromAndroid(Locale.forLanguageTag("pt-BR")))
        assertEquals("pt-br", SiteLocale.fromAndroid(Locale.forLanguageTag("pt-PT")))
    }

    @Test
    fun pickerListsEnglishFirstAndVietnameseLast() {
        assertEquals("en", SiteLocale.SUPPORTED.first())
        assertEquals("vi", SiteLocale.SUPPORTED.last())
        // Every code the picker offers has a real name, not the raw code.
        SiteLocale.SUPPORTED.forEach { assertTrue(it, SiteLocale.nativeName(it) != it) }
    }

    @Test
    fun splitsChineseByScriptAndRegion() {
        assertEquals("zh-cn", SiteLocale.fromAndroid(Locale.forLanguageTag("zh-CN")))
        assertEquals("zh-cn", SiteLocale.fromAndroid(Locale.forLanguageTag("zh-Hans-SG")))
        assertEquals("zh-tw", SiteLocale.fromAndroid(Locale.forLanguageTag("zh-TW")))
        assertEquals("zh-tw", SiteLocale.fromAndroid(Locale.forLanguageTag("zh-HK")))
        assertEquals("zh-tw", SiteLocale.fromAndroid(Locale.forLanguageTag("zh-Hant")))
    }

    @Test
    fun anythingElseFallsBackToTheSitesDefault() {
        assertEquals("en", SiteLocale.fromAndroid(Locale.forLanguageTag("fr-FR")))
    }
}
