package online.phimnet.tv.settings

import java.util.Locale

/**
 * The viewer's locales, and how an Android locale maps onto them.
 *
 * The mapping mirrors parseLocale in viewer/i18n.go, so the television and the
 * website agree on what language a given person gets: Traditional-script or
 * Taiwan/Hong Kong/Macau Chinese is zh-tw, any other Chinese is zh-cn, any
 * Portuguese is pt-br (the site's only Portuguese), the legacy "in" is
 * Indonesian, and the rest match on language alone. Anything the site does not
 * serve falls back to English, which is the site's own default.
 *
 * SUPPORTED is in the site's language-menu order (English first, Vietnamese
 * last) because the Settings picker lists it as is.
 */
object SiteLocale {

    val SUPPORTED: List<String> = listOf(
        "en", "zh-cn", "zh-tw", "ko", "ja", "th", "id", "es", "pt-br", "ru", "vi",
    )

    const val DEFAULT = "en"

    fun fromAndroid(locale: Locale): String {
        val language = locale.language.lowercase()
        if (language == "zh") {
            val traditional = locale.script.equals("Hant", ignoreCase = true) ||
                locale.country.uppercase() in setOf("TW", "HK", "MO")
            return if (traditional) "zh-tw" else "zh-cn"
        }
        // Locale.language already normalizes "id" to the legacy "in" on older
        // Android, so both spellings must land on Indonesian.
        if (language == "in" || language == "id") return "id"
        if (language == "pt") return "pt-br"
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
        "th" -> "ไทย"
        "id" -> "Bahasa Indonesia"
        "es" -> "Español"
        "pt-br" -> "Português (Brasil)"
        "ru" -> "Русский"
        else -> code
    }
}
