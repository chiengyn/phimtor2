#!/usr/bin/env python3
"""Generates tv/app/src/main/res/values*/strings.xml. Run from anywhere:

    python3 tv/tools/sync_strings.py

Where the website already says something, the app reuses the site's own
translation (viewer/locales/*.json) so the two read the same and nothing is
translated twice. App-only strings are listed below with all six translations.

The XML files are OUTPUT: edit this script (or the site's catalogs) and re-run
it, never the XML by hand, or the next sync silently reverts the edit. Like the
site's validateMessages, it refuses to write anything if a locale is missing a
string.
"""
import json
import os
import re
import sys
from xml.sax.saxutils import escape

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
LOCALES = ["vi", "en", "zh-cn", "zh-tw", "ko", "ja"]
# Android resource qualifier per site locale. English is the default (values/).
# Chinese is split by SCRIPT, not region, so zh-HK/zh-MO devices get Traditional
# and zh-SG gets Simplified — the same rule as SiteLocale and the site's
# parseLocale.
QUALIFIER = {"en": "", "vi": "-vi", "zh-cn": "-b+zh+Hans", "zh-tw": "-b+zh+Hant", "ko": "-ko", "ja": "-ja"}

site = {l: json.load(open(f"{REPO}/viewer/locales/{l}.json")) for l in LOCALES}

# name -> site key (reused verbatim)
REUSED = {
    "nav_home": "nav.home",
    "nav_movies": "nav.movies",
    "nav_series": "nav.tv",
    "nav_search": "search.submit",
    "nav_account": "account.label",
    "action_play": "home.play",
    "action_details": "home.info",
    "action_watch_movie": "detail.watch",
    "action_back": "watch.back",
    "action_sign_in": "account.sign_in",
    "action_sign_out": "account.logout",
    "badge_subtitle": "card.subtitle",
    "state_loading": "watch.loading_info",
    "home_empty": "card.none",
    "search_hint": "search.placeholder",
    "search_empty": "card.empty",
    "search_all_genres": "search.all_genres",
    "detail_season": "detail.season",
    "detail_episode": "detail.episode",
    "player_preparing": "watch.preparing_source",
    "player_connecting": "watch.preparing",
    "player_notice": "watch.notice",
    "player_no_peers": "watch.slow",
    "player_reconnecting": "watch.reconnecting",
    "player_compat": "watch.compatibility",
    "player_sources": "watch.sources",
    "player_subtitles": "watch.subtitles",
    "player_sign_in_quality": "watch.sign_in_quality",
    "player_upgrade_quality": "watch.upgrade_quality",
    "player_coming_soon": "watch.coming_soon_title",
    "player_no_source": "watch.no_source",
    "player_free_hint": "watch.free_hint",
    "player_crypto_hint": "watch.crypto_hint",
    "player_play_pause": "watch.play_pause",
}

# name -> {locale: text}; app-only copy.
NEW = {
    "action_retry": {"vi": "Thử lại", "en": "Try again", "zh-cn": "重试", "zh-tw": "重試", "ko": "다시 시도", "ja": "再試行"},
    "action_save": {"vi": "Lưu", "en": "Save", "zh-cn": "保存", "zh-tw": "儲存", "ko": "저장", "ja": "保存"},
    "error_network": {
        "vi": "Không kết nối được máy chủ. Hãy kiểm tra mạng rồi thử lại.",
        "en": "Can't reach the server. Check the connection and try again.",
        "zh-cn": "无法连接服务器。请检查网络后重试。",
        "zh-tw": "無法連線到伺服器。請檢查網路後重試。",
        "ko": "서버에 연결할 수 없습니다. 연결 상태를 확인한 뒤 다시 시도하세요.",
        "ja": "サーバーに接続できません。接続を確認して、もう一度お試しください。",
    },
    "error_server": {
        "vi": "Máy chủ đang gặp sự cố. Vui lòng thử lại.",
        "en": "The server ran into a problem. Please try again.",
        "zh-cn": "服务器出现问题，请重试。",
        "zh-tw": "伺服器發生問題，請重試。",
        "ko": "서버에 문제가 발생했습니다. 다시 시도하세요.",
        "ja": "サーバーで問題が発生しました。もう一度お試しください。",
    },
    "detail_minutes": {"vi": "%d phút", "en": "%d min", "zh-cn": "%d 分钟", "zh-tw": "%d 分鐘", "ko": "%d분", "ja": "%d分"},
    # Phrased as a label and a number so no language needs a plural form.
    "player_peers": {
        "vi": "Peer đã kết nối: %1$d",
        "en": "Connected peers: %1$d",
        "zh-cn": "已连接节点：%1$d",
        "zh-tw": "已連接節點：%1$d",
        "ko": "연결된 피어: %1$d",
        "ja": "接続中のピア：%1$d",
    },
    "player_seek_unavailable": {
        "vi": "Không thể tua trong chế độ tương thích",
        "en": "Seeking isn't available in compatibility mode",
        "zh-cn": "兼容模式下无法快进或快退",
        "zh-tw": "相容模式下無法快轉或倒轉",
        "ko": "호환 모드에서는 탐색할 수 없습니다",
        "ja": "互換モードではシークできません",
    },
    "player_subtitles_off": {"vi": "Tắt", "en": "Off", "zh-cn": "关闭", "zh-tw": "關閉", "ko": "끄기", "ja": "オフ"},
    "player_audio": {"vi": "Âm thanh", "en": "Audio", "zh-cn": "音轨", "zh-tw": "音軌", "ko": "오디오", "ja": "音声"},
    "player_scan_to_upgrade": {
        "vi": "Quét bằng điện thoại để xem các gói",
        "en": "Scan with your phone to see the plans",
        "zh-cn": "用手机扫码查看套餐",
        "zh-tw": "用手機掃碼查看方案",
        "ko": "휴대폰으로 스캔해 요금제를 확인하세요",
        "ja": "スマートフォンでスキャンしてプランを確認",
    },
    "player_next_episode": {"vi": "Tập tiếp theo", "en": "Next episode", "zh-cn": "下一集", "zh-tw": "下一集", "ko": "다음 화", "ja": "次のエピソード"},
    "player_up_next": {"vi": "Tiếp theo", "en": "Up next", "zh-cn": "即将播放", "zh-tw": "即將播放", "ko": "다음 에피소드", "ja": "次に再生"},
    "player_ended": {"vi": "Đã xem xong", "en": "Finished", "zh-cn": "播放结束", "zh-tw": "播放結束", "ko": "재생 완료", "ja": "再生終了"},
    "player_other_quality": {
        "vi": "Xem bản %1$s",
        "en": "Watch in %1$s instead",
        "zh-cn": "改看 %1$s",
        "zh-tw": "改看 %1$s",
        "ko": "%1$s(으)로 보기",
        "ja": "%1$s で見る",
    },
    "link_step_open": {
        "vi": "Trên điện thoại, mở",
        "en": "On your phone, open",
        "zh-cn": "在手机上打开",
        "zh-tw": "在手機上開啟",
        "ko": "휴대폰에서 다음 주소를 여세요",
        "ja": "スマートフォンで次を開く",
    },
    "link_step_code": {
        "vi": "rồi nhập mã này",
        "en": "and enter this code",
        "zh-cn": "然后输入此代码",
        "zh-tw": "然後輸入此代碼",
        "ko": "그리고 이 코드를 입력하세요",
        "ja": "このコードを入力してください",
    },
    "link_scan": {
        "vi": "Hoặc quét bằng camera điện thoại",
        "en": "Or scan with your phone's camera",
        "zh-cn": "或用手机相机扫码",
        "zh-tw": "或用手機相機掃碼",
        "ko": "또는 휴대폰 카메라로 스캔하세요",
        "ja": "またはスマートフォンのカメラでスキャン",
    },
    "link_refresh_note": {
        "vi": "Mã sẽ tự làm mới khi hết hạn.",
        "en": "The code refreshes by itself if it expires.",
        "zh-cn": "代码过期后会自动刷新。",
        "zh-tw": "代碼過期後會自動更新。",
        "ko": "코드가 만료되면 자동으로 새로 고쳐집니다.",
        "ja": "コードの有効期限が切れると自動的に更新されます。",
    },
    "link_idle": {
        "vi": "Mã đã hết hạn. Chọn \"Thử lại\" để lấy mã mới.",
        "en": "The code has expired. Choose \"Try again\" for a new one.",
        "zh-cn": "代码已过期。选择“重试”获取新代码。",
        "zh-tw": "代碼已過期。選擇「重試」取得新代碼。",
        "ko": "코드가 만료되었습니다. 새 코드를 받으려면 \"다시 시도\"를 선택하세요.",
        "ja": "コードの有効期限が切れました。「再試行」を選ぶと新しいコードを取得できます。",
    },
    "link_waiting": {
        "vi": "Đang chờ bạn xác nhận trên điện thoại…",
        "en": "Waiting for you to confirm on your phone…",
        "zh-cn": "正在等待你在手机上确认…",
        "zh-tw": "正在等待你在手機上確認…",
        "ko": "휴대폰에서 확인하기를 기다리는 중…",
        "ja": "スマートフォンでの確認を待っています…",
    },
    "settings_signed_in_as": {
        "vi": "Đã đăng nhập: %1$s",
        "en": "Signed in as %1$s",
        "zh-cn": "已登录：%1$s",
        "zh-tw": "已登入：%1$s",
        "ko": "%1$s(으)로 로그인됨",
        "ja": "%1$s としてログイン中",
    },
    "settings_server": {"vi": "Máy chủ", "en": "Server", "zh-cn": "服务器", "zh-tw": "伺服器", "ko": "서버", "ja": "サーバー"},
    "settings_server_hint": {
        "vi": "Địa chỉ phimnet mà TV này kết nối",
        "en": "The phimnet address this TV connects to",
        "zh-cn": "此电视连接的 phimnet 地址",
        "zh-tw": "此電視連線的 phimnet 位址",
        "ko": "이 TV가 연결하는 phimnet 주소",
        "ja": "このテレビが接続する phimnet のアドレス",
    },
    "settings_server_invalid": {
        "vi": "Đây không phải là một địa chỉ web hợp lệ.",
        "en": "That doesn't look like a web address.",
        "zh-cn": "这不是有效的网址。",
        "zh-tw": "這不是有效的網址。",
        "ko": "올바른 웹 주소가 아닙니다.",
        "ja": "有効なウェブアドレスではありません。",
    },
    "settings_server_https": {
        "vi": "Hãy dùng địa chỉ https://.",
        "en": "Use an https:// address.",
        "zh-cn": "请使用 https:// 地址。",
        "zh-tw": "請使用 https:// 位址。",
        "ko": "https:// 주소를 사용하세요.",
        "ja": "https:// のアドレスを使用してください。",
    },
    "settings_server_note": {
        "vi": "Đổi máy chủ sẽ đăng xuất TV này.",
        "en": "Changing the server signs this TV out.",
        "zh-cn": "更换服务器会使这台电视退出登录。",
        "zh-tw": "更換伺服器會讓這台電視登出。",
        "ko": "서버를 변경하면 이 TV에서 로그아웃됩니다.",
        "ja": "サーバーを変更すると、このテレビはログアウトします。",
    },
    "settings_language": {
        "vi": "Ngôn ngữ nội dung",
        "en": "Content language",
        "zh-cn": "内容语言",
        "zh-tw": "內容語言",
        "ko": "콘텐츠 언어",
        "ja": "コンテンツの言語",
    },
    "settings_language_auto": {
        "vi": "Theo ngôn ngữ của TV",
        "en": "Same as the TV",
        "zh-cn": "跟随电视",
        "zh-tw": "跟隨電視",
        "ko": "TV 설정과 동일",
        "ja": "テレビと同じ",
    },
    "settings_app": {
        "vi": 'Ứng dụng',
        "en": 'App',
        "zh-cn": '应用',
        "zh-tw": '應用程式',
        "ko": '앱',
        "ja": 'アプリ',
    },
    "update_available": {
        "vi": 'Có bản cập nhật: %1$s',
        "en": 'Update available: %1$s',
        "zh-cn": '有可用更新：%1$s',
        "zh-tw": '有可用更新：%1$s',
        "ko": '업데이트 가능: %1$s',
        "ja": 'アップデートがあります：%1$s',
    },
    "update_now": {
        "vi": 'Cập nhật',
        "en": 'Update',
        "zh-cn": '更新',
        "zh-tw": '更新',
        "ko": '업데이트',
        "ja": 'アップデート',
    },
    "update_later": {
        "vi": 'Để sau',
        "en": 'Later',
        "zh-cn": '稍后',
        "zh-tw": '稍後',
        "ko": '나중에',
        "ja": '後で',
    },
    "update_downloading": {
        "vi": 'Đang tải bản cập nhật… %1$d%%',
        "en": 'Downloading update… %1$d%%',
        "zh-cn": '正在下载更新… %1$d%%',
        "zh-tw": '正在下載更新… %1$d%%',
        "ko": '업데이트 다운로드 중… %1$d%%',
        "ja": 'アップデートをダウンロード中… %1$d%%',
    },
    # Android closes the app to replace it and will not let the new version
    # reopen itself, so say so before it happens (see update/Updater.kt).
    "update_installing": {
        "vi": 'Đang cài đặt bản cập nhật… phimnet sẽ đóng lại — hãy mở lại từ màn hình chính.',
        "en": 'Installing the update… phimnet will close — open it again from the home screen.',
        "zh-cn": '正在安装更新… phimnet 将会关闭，请从主屏幕重新打开。',
        "zh-tw": '正在安裝更新… phimnet 將會關閉，請從主畫面重新開啟。',
        "ko": '업데이트 설치 중… phimnet이 종료됩니다. 홈 화면에서 다시 여세요.',
        "ja": 'アップデートをインストール中… phimnet は終了します。ホーム画面から再度開いてください。',
    },
    "update_permission": {
        "vi": 'Hãy cho phép phimnet cài đặt ứng dụng trong phần cài đặt của TV, rồi quay lại để tiếp tục.',
        "en": "Allow phimnet to install apps in the TV's settings, then come back to continue.",
        "zh-cn": '请在电视设置中允许 phimnet 安装应用，然后返回继续。',
        "zh-tw": '請在電視設定中允許 phimnet 安裝應用程式，然後返回繼續。',
        "ko": 'TV 설정에서 phimnet의 앱 설치를 허용한 뒤 돌아와서 계속하세요.',
        "ja": 'テレビの設定で phimnet によるアプリのインストールを許可してから、戻って続けてください。',
    },
    "update_open_settings": {
        "vi": 'Mở cài đặt',
        "en": 'Open settings',
        "zh-cn": '打开设置',
        "zh-tw": '開啟設定',
        "ko": '설정 열기',
        "ja": '設定を開く',
    },
    "update_continue": {
        "vi": 'Tiếp tục',
        "en": 'Continue',
        "zh-cn": '继续',
        "zh-tw": '繼續',
        "ko": '계속',
        "ja": '続ける',
    },
    "update_no_settings": {
        "vi": 'Không mở được cài đặt. Bạn có thể cập nhật bằng Downloader tại %1$s',
        "en": "Couldn't open settings. You can also update with Downloader from %1$s",
        "zh-cn": '无法打开设置。你也可以用 Downloader 从 %1$s 更新',
        "zh-tw": '無法開啟設定。你也可以用 Downloader 從 %1$s 更新',
        "ko": '설정을 열 수 없습니다. Downloader로 %1$s 에서 업데이트할 수도 있습니다',
        "ja": '設定を開けませんでした。Downloader で %1$s から更新することもできます',
    },
    "update_corrupt": {
        "vi": 'Bản tải về bị lỗi. Vui lòng thử lại.',
        "en": 'The download was damaged. Please try again.',
        "zh-cn": '下载的文件已损坏，请重试。',
        "zh-tw": '下載的檔案已損毀，請重試。',
        "ko": '다운로드한 파일이 손상되었습니다. 다시 시도하세요.',
        "ja": 'ダウンロードしたファイルが破損しています。もう一度お試しください。',
    },
    "update_install_failed": {
        "vi": 'Không cài được bản cập nhật.',
        "en": "The update couldn't be installed.",
        "zh-cn": '无法安装更新。',
        "zh-tw": '無法安裝更新。',
        "ko": '업데이트를 설치할 수 없습니다.',
        "ja": 'アップデートをインストールできませんでした。',
    },
    "update_check": {
        "vi": 'Kiểm tra cập nhật',
        "en": 'Check for updates',
        "zh-cn": '检查更新',
        "zh-tw": '檢查更新',
        "ko": '업데이트 확인',
        "ja": 'アップデートを確認',
    },
    "update_checking": {
        "vi": 'Đang kiểm tra…',
        "en": 'Checking…',
        "zh-cn": '正在检查…',
        "zh-tw": '正在檢查…',
        "ko": '확인 중…',
        "ja": '確認中…',
    },
    "update_up_to_date": {
        "vi": 'Bạn đang dùng bản mới nhất.',
        "en": "You're on the latest version.",
        "zh-cn": '已是最新版本。',
        "zh-tw": '已是最新版本。',
        "ko": '최신 버전을 사용 중입니다.',
        "ja": '最新バージョンです。',
    },
    "update_check_failed": {
        "vi": 'Không kiểm tra được bản cập nhật.',
        "en": "Couldn't check for updates.",
        "zh-cn": '无法检查更新。',
        "zh-tw": '無法檢查更新。',
        "ko": '업데이트를 확인할 수 없습니다.',
        "ja": 'アップデートを確認できませんでした。',
    },
    "settings_version": {
        "vi": "Phiên bản %1$s",
        "en": "Version %1$s",
        "zh-cn": "版本 %1$s",
        "zh-tw": "版本 %1$s",
        "ko": "버전 %1$s",
        "ja": "バージョン %1$s",
    },
}


def android_escape(text: str) -> str:
    """Escapes a string for an Android <string> element."""
    text = escape(text)  # & < >
    text = text.replace("\\", "\\\\").replace("'", "\\'").replace('"', '\\"')
    if text.startswith(("@", "?")):
        text = "\\" + text
    return text


def typography(text: str) -> str:
    """The site writes "..." in places; Android's typography lint wants "…"."""
    return text.replace("...", "…")


def positional(text: str) -> str:
    """Rewrites a lone %s / %d to %1$s / %1$d, which aapt prefers."""
    if len(re.findall(r"%[sd]", text)) == 1:
        text = re.sub(r"%([sd])", r"%1$\1", text)
    return text


overlap = set(REUSED) & set(NEW)
if overlap:
    sys.exit(f"defined twice: {overlap}")

for locale in LOCALES:
    rows = ['    <string name="app_name" translatable="false">phimnet</string>'] if locale == "en" else []
    for name, key in sorted(REUSED.items()):
        value = site[locale].get(key)
        if value is None:
            sys.exit(f"site key {key} missing for {locale}")
        rows.append(f'    <string name="{name}">{android_escape(positional(typography(value)))}</string>')
    for name, translations in sorted(NEW.items()):
        if locale not in translations:
            sys.exit(f"{name} has no {locale} translation")
        rows.append(f'    <string name="{name}">{android_escape(positional(typography(translations[locale])))}</string>')
    rows.sort(key=lambda r: re.search(r'name="([^"]+)"', r).group(1))

    out_dir = f"{REPO}/tv/app/src/main/res/values{QUALIFIER[locale]}"
    os.makedirs(out_dir, exist_ok=True)
    with open(f"{out_dir}/strings.xml", "w", encoding="utf-8") as f:
        f.write('<?xml version="1.0" encoding="utf-8"?>\n')
        f.write("<!-- GENERATED by tv/tools/sync_strings.py from viewer/locales/*.json. Do not edit. -->\n")
        f.write("<resources>\n")
        f.write("\n".join(rows))
        f.write("\n</resources>\n")
    print(f"{locale:6} -> values{QUALIFIER[locale]:12} {len(rows)} strings")
