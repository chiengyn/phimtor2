package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/language"
)

type Locale string

const (
	LocaleVI   Locale = "vi"
	LocaleEN   Locale = "en"
	LocaleZHCN Locale = "zh-cn"
	LocaleZHTW Locale = "zh-tw"
	LocaleKO   Locale = "ko"
	LocaleJA   Locale = "ja"
)

type localeDefinition struct {
	Locale          Locale
	NativeName      string
	OpenGraphLocale string
}

// localeDefinitions is the registry behind routing, template parsing, SEO, and
// the language dropdown. Adding a locale here (plus its message catalog and
// parser case) makes it appear in the menu without changing layout markup.
var localeDefinitions = []localeDefinition{
	{Locale: LocaleVI, NativeName: "Tiếng Việt", OpenGraphLocale: "vi_VN"},
	{Locale: LocaleEN, NativeName: "English", OpenGraphLocale: "en_US"},
	{Locale: LocaleZHCN, NativeName: "简体中文", OpenGraphLocale: "zh_CN"},
	{Locale: LocaleZHTW, NativeName: "繁體中文", OpenGraphLocale: "zh_TW"},
	{Locale: LocaleKO, NativeName: "한국어", OpenGraphLocale: "ko_KR"},
	{Locale: LocaleJA, NativeName: "日本語", OpenGraphLocale: "ja_JP"},
}

var supportedLocales = func() []Locale {
	locales := make([]Locale, 0, len(localeDefinitions))
	for _, definition := range localeDefinitions {
		locales = append(locales, definition.Locale)
	}
	return locales
}()

type localeContextKey struct{}

type languageOption struct {
	Locale  Locale
	Name    string
	URL     string
	Current bool
}

func languageName(locale Locale) string {
	for _, definition := range localeDefinitions {
		if definition.Locale == locale {
			return definition.NativeName
		}
	}
	return string(locale)
}

func openGraphLocale(locale Locale) string {
	for _, definition := range localeDefinitions {
		if definition.Locale == locale {
			return definition.OpenGraphLocale
		}
	}
	return string(locale)
}

func parseLocale(raw string) (Locale, bool) {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "_", "-")
	switch {
	case normalized == "vi" || strings.HasPrefix(normalized, "vi-"):
		return LocaleVI, true
	case normalized == "en" || strings.HasPrefix(normalized, "en-"):
		return LocaleEN, true
	case normalized == "zh-tw" || strings.HasPrefix(normalized, "zh-tw-") ||
		normalized == "zh-hant" || strings.HasPrefix(normalized, "zh-hant-") ||
		normalized == "zh-hk" || strings.HasPrefix(normalized, "zh-hk-") ||
		normalized == "zh-mo" || strings.HasPrefix(normalized, "zh-mo-"):
		return LocaleZHTW, true
	case normalized == "zh" || normalized == "zh-cn" || strings.HasPrefix(normalized, "zh-cn-") ||
		normalized == "zh-hans" || strings.HasPrefix(normalized, "zh-hans-") ||
		normalized == "zh-sg" || strings.HasPrefix(normalized, "zh-sg-"):
		return LocaleZHCN, true
	case normalized == "ko" || strings.HasPrefix(normalized, "ko-"):
		return LocaleKO, true
	case normalized == "ja" || strings.HasPrefix(normalized, "ja-"):
		return LocaleJA, true
	default:
		return "", false
	}
}

func localeFromContext(ctx context.Context) Locale {
	if locale, ok := ctx.Value(localeContextKey{}).(Locale); ok {
		return locale
	}
	return LocaleVI
}

func fallbackLocale(locale Locale) Locale {
	if locale == LocaleEN {
		return LocaleVI
	}
	// The catalog currently stores Vietnamese and English metadata. New UI
	// locales use English metadata until matching TMDB translations are present.
	return LocaleEN
}

func localeOrigin(locale Locale) string { return "/" + string(locale) }

func localeHome(locale Locale) string { return localeOrigin(locale) + "/" }

func localeFromRequestPath(path string) Locale {
	for _, locale := range supportedLocales {
		prefix := localeOrigin(locale)
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return locale
		}
	}
	return LocaleVI
}

// localizedRequestURL keeps the visitor on the same page and query while
// replacing only the explicit locale prefix. It is intentionally generic so a
// newly supported locale automatically participates in the language menu.
func localizedRequestURL(r *http.Request, target Locale) string {
	current := localeFromContext(r.Context())
	path := r.URL.Path
	prefix := localeOrigin(current)
	if path == prefix {
		path = localeOrigin(target)
	} else if strings.HasPrefix(path, prefix+"/") {
		path = localeOrigin(target) + strings.TrimPrefix(path, prefix)
	} else {
		path = localeHome(target)
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	return path
}

func requestLanguageOptions(r *http.Request) []languageOption {
	current := localeFromContext(r.Context())
	options := make([]languageOption, 0, len(supportedLocales))
	for _, locale := range supportedLocales {
		options = append(options, languageOption{
			Locale: locale, Name: languageName(locale),
			URL: localizedRequestURL(r, locale), Current: locale == current,
		})
	}
	return options
}

func preferredLocale(r *http.Request) Locale {
	if cookie, err := r.Cookie("phimnet_locale"); err == nil {
		if locale, ok := parseLocale(cookie.Value); ok {
			return locale
		}
	}
	tags, _, err := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	if err == nil {
		for _, tag := range tags {
			locale, ok := parseLocale(tag.String())
			if ok {
				return locale
			}
		}
	}
	// Be forgiving of a malformed header: browsers normally emit valid syntax,
	// but a simple first recognizable language still gives a sensible result.
	for _, item := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		if locale, ok := parseLocale(strings.TrimSpace(strings.SplitN(item, ";", 2)[0])); ok {
			return locale
		}
	}
	return LocaleVI
}

func setLocaleCookie(w http.ResponseWriter, r *http.Request, locale Locale) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: "phimnet_locale", Value: string(locale), Path: "/",
		MaxAge: int((365 * 24 * time.Hour).Seconds()), SameSite: http.SameSiteLaxMode,
		Secure: secure, HttpOnly: true,
	})
}

func localizeLegacyURL(locale Locale, r *http.Request) string {
	path := localeOrigin(locale) + r.URL.Path
	if r.URL.Path == "/" {
		path = localeHome(locale)
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	return path
}

func localeURL(locale Locale, path string) string {
	if path == "" || path == "/" {
		return localeHome(locale)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return localeOrigin(locale) + path
}

func localeQueryURL(locale Locale, path string, values url.Values) string {
	out := localeURL(locale, path)
	if encoded := values.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out
}

func formatDate(locale Locale, value time.Time) string {
	switch locale {
	case LocaleEN:
		return value.Format("Jan 2, 2006")
	case LocaleZHCN, LocaleZHTW, LocaleJA:
		return value.Format("2006年1月2日")
	case LocaleKO:
		return value.Format("2006년 1월 2일")
	default:
		return value.Format("02/01/2006")
	}
}

// localeCatalogs packages translation files into the viewer binary while keeping
// copy separate from application code.
//
//go:embed locales/*.json
var localeCatalogs embed.FS

var (
	messagesOnce    sync.Once
	messages        map[Locale]map[string]string
	messagesLoadErr error
)

func loadMessages() error {
	messagesOnce.Do(func() {
		loaded := make(map[Locale]map[string]string, len(localeDefinitions))
		for _, definition := range localeDefinitions {
			path := "locales/" + string(definition.Locale) + ".json"
			payload, err := localeCatalogs.ReadFile(path)
			if err != nil {
				messagesLoadErr = fmt.Errorf("read translation catalog %s: %w", path, err)
				return
			}
			catalog := map[string]string{}
			if err := json.Unmarshal(payload, &catalog); err != nil {
				messagesLoadErr = fmt.Errorf("decode translation catalog %s: %w", path, err)
				return
			}
			if len(catalog) == 0 {
				messagesLoadErr = fmt.Errorf("translation catalog %s is empty", path)
				return
			}
			loaded[definition.Locale] = catalog
		}
		messages = loaded
	})
	return messagesLoadErr
}

func tr(locale Locale, key string, args ...any) string {
	if err := loadMessages(); err != nil {
		return key
	}
	value := messages[locale][key]
	if value == "" {
		value = messages[LocaleVI][key]
	}
	if value == "" {
		value = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(value, args...)
	}
	return value
}

func validateMessages() error {
	if err := loadMessages(); err != nil {
		return err
	}
	reference, ok := messages[LocaleVI]
	if !ok {
		return fmt.Errorf("default translation catalog %q is missing", LocaleVI)
	}
	for _, locale := range supportedLocales {
		catalog, ok := messages[locale]
		if !ok {
			return fmt.Errorf("translation catalog %q is missing", locale)
		}
		for key := range reference {
			if _, ok := catalog[key]; !ok {
				return fmt.Errorf("%s translation missing key %q", locale, key)
			}
		}
		for key := range catalog {
			if _, ok := reference[key]; !ok {
				return fmt.Errorf("%s translation has unknown key %q", locale, key)
			}
		}
	}
	return nil
}
