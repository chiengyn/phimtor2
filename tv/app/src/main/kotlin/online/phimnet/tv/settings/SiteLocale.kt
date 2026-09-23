package online.phimnet.tv.settings

import java.util.Locale

/**
 * The viewer's locales, and how an Android locale maps onto them.
 *
 * The mapping mirrors parseLocale in viewer/i18n.go, so the television and the
 * website agree on what language a given person gets: Traditional-script or
 * Taiwan/Hong Kong/Macau Chinese is zh-tw, any other Chinese is zh-cn, and the
 * rest match on language alone. Anything the site does not serve falls back to
 * Vietnamese, which is the site's own default.
 */
object SiteLocale {

    val SUPPORTED: List<String> = listOf("vi", "en", "zh-cn", "zh-tw", "ko", "ja")

    const val DEFAULT = "vi"

    fun fromAndroid(locale: Locale): String {
        val language = locale.language.lowercase()
        if (language == "zh") {
            val traditional = locale.script.equals("Hant", ignoreCase = true) ||
                locale.country.uppercase() in setOf("TW", "HK", "MO")
            return if (traditional) "zh-tw" else "zh-cn"
        }
        return language.takeIf { it in SUPPORTED } ?: DEFAULT
    }

    /** The language's own name, for the picker — the same names the site's menu uses. */
    fun nativeName(code: String): String = when (code) {
        "vi" -> "Tiếng Việt"
        "en" -> "English"
        "zh-cn" -> "简体中文"
        "zh-tw" -> "繁體中文"
        "ko" -> "한국어"
        "ja" -> "日本語"
        else -> code
    }
}
