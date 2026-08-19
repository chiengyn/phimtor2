// mkvattach.js — the admin's shared glue between a <video> and the streamer.
//
// The viewer solves the same problem inline in its watch page, but it cannot know
// the container (its browser-facing DTO deliberately omits the file path), so it
// sniffs the raw stream's Content-Type. Every admin surface *does* have the path
// already, so the decision here is a cheap extension check with no extra request.
//
// mkvplayer.js is a byte-for-byte copy of viewer/static/mkvplayer.js — the two
// services duplicate rather than share (same convention as pickVideoFile and the
// videoExtensions tables). Keep them in sync.

import { attachRemuxedSource } from './mkvplayer.js';

// Containers a browser demuxes on its own. This mirrors browserNativeExts in the
// streamer's transcode.go — if one changes, change the other.
const NATIVE_EXTS = ['mp4', 'm4v', 'webm', 'ogg', 'ogv'];

export function needsClientRemux(filePath) {
	const ext = (String(filePath || '').split('.').pop() || '').toLowerCase();
	return ext !== '' && !NATIVE_EXTS.includes(ext);
}

/**
 * Point a <video> at a streamer file.
 *
 * Browser-native containers are attached directly. Anything else (.mkv above all)
 * is demuxed in the page and remuxed to fragmented MP4, which is what makes it
 * seekable; if the codecs inside cannot be decoded (AC3/E-AC3/DTS audio, HEVC with
 * no hardware decoder) it falls back once to the streamer's ffmpeg transcode.
 *
 * @param {HTMLVideoElement} video
 * @param {string} streamURL  the plain stream URL, no query string
 * @param {string} filePath   the file's path inside the torrent (for its extension)
 * @param {{onStatus?: (msg: string) => void}} opts
 * @returns {Promise<{destroy: () => void}>}
 */
export async function attachSource(video, streamURL, filePath, opts = {}) {
	const { onStatus = () => {} } = opts;
	let controller = null;
	let fellBack = false;

	const destroy = () => {
		if (controller) { try { controller.destroy(); } catch (e) {} controller = null; }
	};

	if (!needsClientRemux(filePath)) {
		video.src = streamURL;
		video.play().catch(() => {});
		return { destroy };
	}

	// One-shot: if the transcode itself fails we must not bounce back to the
	// remuxer, or the two would ping-pong.
	const fallback = () => {
		if (fellBack) return;
		fellBack = true;
		destroy();
		onStatus('Định dạng chưa được hỗ trợ trực tiếp, đang chuyển sang chế độ tương thích…');
		video.querySelectorAll('source').forEach((el) => el.remove());
		video.removeAttribute('src');
		video.src = streamURL; // no ?raw=1 — the streamer's ffmpeg path
		try { video.load(); video.play().catch(() => {}); } catch (e) {}
	};

	// MEDIA_ERR_SRC_NOT_SUPPORTED is a codec problem, not a transport one, so a
	// reload would never fix it.
	video.addEventListener('error', () => {
		if (video.error && video.error.code === 4) fallback();
	});

	try {
		controller = await attachRemuxedSource(video, streamURL + '?raw=1', {
			onStatus,
			onLog: (info) => console.log('[mkvplayer] probe', info),
		});
		if (!video.isConnected) { destroy(); return { destroy }; }
		video.play().catch(() => {});
	} catch (e) {
		console.warn('[admin] client remux unavailable:', (e && e.message) || e);
		if (video.isConnected) fallback();
	}
	return { destroy };
}
