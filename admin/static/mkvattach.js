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
const activeAttachments = new WeakMap();

export async function attachSource(video, streamURL, filePath, opts = {}) {
	const { onStatus = () => {} } = opts;
	activeAttachments.get(video)?.destroy();
	const abort = new AbortController();
	let controller = null;
	let fellBack = false;
	let destroyed = false;

	const destroy = () => {
		if (destroyed) return;
		destroyed = true;
		abort.abort();
		video.removeEventListener('error', onVideoError);
		controller?.destroy();
		controller = null;
		if (activeAttachments.get(video) === attachment) activeAttachments.delete(video);
	};
	const attachment = { destroy };
	activeAttachments.set(video, attachment);

	const fallback = () => {
		if (destroyed || fellBack || !video.isConnected) return;
		fellBack = true;
		abort.abort();
		controller?.destroy();
		controller = null;
		onStatus('Định dạng chưa được hỗ trợ trực tiếp, đang chuyển sang chế độ tương thích…');
		video.querySelectorAll('source').forEach((el) => el.remove());
		const url = new URL(streamURL, document.baseURI);
		url.searchParams.delete('raw');
		url.searchParams.set('transcode', '1');
		video.src = url.href;
		try { video.load(); video.play().catch(() => {}); } catch (e) {}
	};

	function onVideoError() {
		if ([3, 4].includes(video.error?.code)) fallback();
	}
	video.addEventListener('error', onVideoError);

	if (!needsClientRemux(filePath)) {
		video.src = streamURL;
		video.play().catch(() => {});
		return attachment;
	}

	try {
		const rawURL = new URL(streamURL, document.baseURI);
		rawURL.searchParams.set('raw', '1');
		const result = await attachRemuxedSource(video, rawURL.href, {
			onStatus,
			onLog: (info) => console.log('[mkvplayer] probe', info),
			onError: fallback,
			signal: abort.signal,
		});
		if (destroyed || fellBack || !video.isConnected) { result.destroy(); return attachment; }
		controller = result;
		video.play().catch(() => {});
	} catch (e) {
		if (!abort.signal.aborted) {
			console.warn('[admin] client remux unavailable:', (e && e.message) || e);
			fallback();
		}
	}
	return attachment;
}
