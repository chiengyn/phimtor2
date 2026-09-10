// mkvplayer.js — play a container the browser cannot demux (.mkv and friends) by
// remuxing it to fragmented MP4 in the page and feeding that to MediaSource.
//
// DUPLICATED: viewer/static/mkvplayer.js and admin/static/mkvplayer.js are kept
// byte-for-byte identical (the two services duplicate rather than share a package,
// as they already do for their query layers, and go:embed cannot reach across
// module dirs). Change one, copy to the other.
//
// Why this exists: browsers do not demux Matroska. Chrome handles only the WebM
// subset (VP8/VP9/AV1 + Opus/Vorbis) and refuses H.264-in-MKV; Firefox and Safari
// have no Matroska support at all. The streamer can remux server-side with ffmpeg,
// but that output is a non-seekable chunked pipe — a scrub has to wait for the
// stream to sequentially reach the target. Doing the remux here instead means the
// bytes arrive over ordinary HTTP range requests, so seeking is just another range
// request and the streamer's PrioritizeSeek warms the right pieces.
//
// This only ever *re*muxes: encoded packets are passed through untouched. That
// makes it cheap, but it also means the browser must already be able to decode the
// codecs inside. AC3/E-AC3/DTS audio and HEVC without a hardware decoder cannot be
// rescued by repackaging, so those are detected up front and reported via
// UnsupportedMediaError — the caller then falls back to the server's ffmpeg path.

const MEDIABUNNY_URL = 'https://cdn.jsdelivr.net/npm/mediabunny@1.55.1/dist/bundles/mediabunny.min.mjs';

// How far ahead of the playhead to keep the SourceBuffer filled, and how much
// history to drop when the browser refuses more data. MSE buffers are finite and
// a 4K remux fills them fast, so the pump idles once it is far enough ahead.
const BUFFER_AHEAD_SEC = 60;
const BUFFER_BEHIND_SEC = 30;
const MAX_QUEUED_BYTES = 8 * 1024 * 1024;

// A seek inside already-buffered data is handled by the media element itself; only
// a seek beyond this tolerance of the buffered ranges is worth tearing the muxing
// session down and restarting it at a new keyframe.
const SEEK_TOLERANCE_SEC = 0.5;

// Thrown when the file's codecs cannot be played by repackaging alone. The caller
// treats this as "fall back to the server transcode", not as an error to show.
export class UnsupportedMediaError extends Error {
	constructor(message) {
		super(message);
		this.name = 'UnsupportedMediaError';
	}
}

// Mediabunny is ~650 KB, so it is loaded on demand: an .mp4 source never pays for
// it. The promise is cached so a source switch reuses the already-loaded module.
let mediabunnyPromise = null;
function loadMediabunny() {
	if (!mediabunnyPromise) {
		mediabunnyPromise = import(MEDIABUNNY_URL).catch((err) => {
			mediabunnyPromise = null; // allow retry after a transient CDN failure
			throw err;
		});
	}
	return mediabunnyPromise;
}

function concatBytes(a, b) {
	const out = new Uint8Array(a.length + b.length);
	out.set(a, 0);
	out.set(b, a.length);
	return out;
}

// True if t falls inside one of the element's buffered ranges (with a little slack
// at the end, since a seek to the very edge of the buffer still needs more data).
function isBuffered(ranges, t) {
	for (let i = 0; i < ranges.length; i++) {
		if (t >= ranges.start(i) && t < ranges.end(i) - 0.1) return true;
	}
	return false;
}

// bufferedAhead reports how many seconds of contiguous data sit after t.
function bufferedAhead(ranges, t) {
	for (let i = 0; i < ranges.length; i++) {
		if (t >= ranges.start(i) && t <= ranges.end(i)) return ranges.end(i) - t;
	}
	return 0;
}

/**
 * Attach a remuxed MediaSource to a <video> element.
 *
 * Probes the container first and throws UnsupportedMediaError *before* touching
 * the element if the codecs can't be played, so the caller can fall back cleanly
 * without the user ever seeing a broken player.
 *
 * @param {HTMLVideoElement} video
 * @param {string} rawURL  streamer stream URL with ?raw=1
 * @param {{onStatus?: (msg: string) => void, onLog?: (info: object) => void, onError?: (error: Error) => void, signal?: AbortSignal}} opts
 * @returns {Promise<{destroy: () => void}>}
 */
export async function attachRemuxedSource(video, rawURL, opts = {}) {
	const { onStatus = () => {}, onLog = () => {}, onError = () => {}, signal } = opts;
	signal?.throwIfAborted();

	if (typeof window.MediaSource === 'undefined') {
		throw new UnsupportedMediaError('MediaSource is not available in this browser');
	}

	onStatus('Đang phân tích định dạng video…');
	const MB = await loadMediabunny();

	const input = new MB.Input({
		source: new MB.UrlSource(rawURL),
		formats: MB.ALL_FORMATS,
	});

	const dispose = () => input.dispose();
	signal?.addEventListener('abort', dispose, { once: true });
	try {
		signal?.throwIfAborted();
		const videoTrack = await input.getPrimaryVideoTrack();
		const audioTrack = await input.getPrimaryAudioTrack();
		if (!videoTrack) throw new UnsupportedMediaError('container has no video track');

		// This path passes encoded packets directly to MSE. WebCodecs canDecode()
		// tests a different API and can reject codecs that <video>/MSE can play.
		// Let MSE advertise support, then handle actual decode errors via fallback.
		const codecInfo = {};
		for (const [label, track] of [['video', videoTrack], ['audio', audioTrack]]) {
			if (!track) continue;
			const codec = await track.getCodec();
			if (!codec) throw new UnsupportedMediaError(`Unknown ${label} codec`);
			codecInfo[label] = codec;
		}

		const codecStrings = [];
		for (const track of [videoTrack, audioTrack]) {
			if (!track) continue;
			const s = await track.getCodecParameterString();
			if (!s) throw new UnsupportedMediaError('Missing codec parameters');
			codecStrings.push(s);
		}
		const mimeType = `video/mp4; codecs="${codecStrings.join(',')}"`;
		if (!window.MediaSource.isTypeSupported(mimeType)) {
			throw new UnsupportedMediaError(`MediaSource will not accept ${mimeType}`);
		}

		const duration = await input.computeDuration();
		onLog({ mimeType, duration, codecs: codecInfo, format: (await input.getFormat())?.name });

		signal?.throwIfAborted();
		if (!video.isConnected) throw new DOMException('Player was removed', 'AbortError');
		return startPlayback({ MB, video, input, videoTrack, audioTrack, mimeType, duration, onStatus, onError, signal });
	} catch (err) {
		input.dispose();
		throw err;
	} finally {
		signal?.removeEventListener('abort', dispose);
	}
}

function startPlayback({ MB, video, input, videoTrack, audioTrack, mimeType, duration, onStatus, onError, signal }) {
	const mediaSource = new MediaSource();
	const objectURL = URL.createObjectURL(mediaSource);

	let sourceBuffer = null;
	let session = null;      // the in-flight muxing session, if any
	let generation = 0;      // bumped on every restart so stale sessions self-cancel
	let destroyed = false;

	// Appends must be serialized: a SourceBuffer accepts one operation at a time.
	const appendQueue = [];
	let appending = false;
	let queuedBytes = 0;
	let quotaBlocked = false;
	let outputEnded = false;

	function fail(err) {
		if (destroyed) return;
		destroy();
		onError(err);
	}

	function maybeEndStream() {
		if (!destroyed && outputEnded && !appendQueue.length && sourceBuffer &&
			!sourceBuffer.updating && mediaSource.readyState === 'open') {
			try { mediaSource.endOfStream(); } catch (err) { fail(err); }
		}
	}

	function drainQueue() {
		if (destroyed || appending || !appendQueue.length) return;
		if (!sourceBuffer || sourceBuffer.updating) return;
		if (mediaSource.readyState !== 'open') return;

		const chunk = appendQueue[0];
		appending = true;
		try {
			sourceBuffer.appendBuffer(chunk.bytes);
			quotaBlocked = false;
		} catch (err) {
			appending = false;
			if (err && err.name === 'QuotaExceededError') {
				// The buffer is full. Drop history well behind the playhead and retry
				// the same chunk on the next tick rather than dropping it.
				quotaBlocked = true;
				evictBehind(true);
				return;
			}
			fail(err);
		}
	}

	function evictBehind(urgent = false) {
		if (!sourceBuffer || sourceBuffer.updating) return;
		const cutoff = video.currentTime - (urgent ? 1 : BUFFER_BEHIND_SEC);
		if (cutoff <= 0 || !sourceBuffer.buffered.length || sourceBuffer.buffered.start(0) >= cutoff) return;
		try {
			sourceBuffer.remove(0, cutoff);
		} catch (err) { /* racing another op; the next drain will retry */ }
	}

	function enqueue(bytes, meta) {
		queuedBytes += bytes.byteLength;
		appendQueue.push({ bytes, ...meta });
		drainQueue();
	}

	function onUpdateEnd() {
		// updateend fires for remove() as well as appendBuffer(). Only an append we
		// started consumes a queue entry — shifting on a remove would silently drop a
		// segment that was never written.
		if (!appending) {
			drainQueue();
			maybeEndStream();
			return;
		}
		const done = appendQueue.shift();
		if (done) queuedBytes -= done.bytes.byteLength;
		appending = false;
		if (done && done.onAppended) {
			try { done.onAppended(); } catch (err) { /* calibration is best-effort */ }
		}
		drainQueue();
		unstickSeek();
		maybeEndStream();
	}

	// Chrome will not finish a seek whose target lands exactly on the start of a
	// buffered range — it wants a frame that strictly contains the instant, so the
	// element sits in seeking/HAVE_METADATA forever even though data is arriving.
	// Calibration keeps the range starting before the target, but a seek to a time
	// that *is* the first keyframe (0, most often) still hits it. Nudge just inside.
	function unstickSeek() {
		if (destroyed || !video.seeking || video.readyState >= 2) return;
		const ranges = video.buffered;
		const t = video.currentTime;
		for (let i = 0; i < ranges.length; i++) {
			const start = ranges.start(i);
			if (start >= t && start - t < SEEK_TOLERANCE_SEC && ranges.end(i) - start > 0.1) {
				// Re-enters onSeeking, which sees the new target as buffered and no-ops.
				video.currentTime = start + 0.05;
				return;
			}
		}
	}

	// --- muxing session ----------------------------------------------------
	//
	// One session = one fragmented-MP4 Output covering a contiguous run of playback.
	// A seek outside the buffer ends the current session and opens a new one at the
	// preceding keyframe, because a fragmented MP4 can only be written forward.

	async function startSession(startTime) {
		const gen = ++generation;
		const cancelled = () => destroyed || gen !== generation;

		let ftyp = null;
		let pendingMoof = null;
		// Held so it can be re-appended if the timeline needs calibrating (below).
		let firstSegment = null;
		let calibrated = startTime === 0;
		// The pump starts at the last keyframe at or *before* startTime, which can be
		// a full GOP earlier. Calibration has to compare against that timestamp, not
		// the seek target — see calibrateTimeline.
		const anchor = { time: startTime };

		const output = new MB.Output({
			// The bytes are consumed through the box callbacks below, which is what
			// splits the stream into an MSE init segment and media segments; the
			// target itself just has to exist.
			target: new MB.StreamTarget(new WritableStream({ write() {} })),
			format: new MB.Mp4OutputFormat({
				fastStart: 'fragmented',
				onFtyp: (data) => { ftyp = data.slice(); },
				onMoov: (data) => {
					// ftyp + moov is the MSE initialization segment.
					if (cancelled()) return;
					enqueue(concatBytes(ftyp || new Uint8Array(0), data.slice()));
				},
				onMoof: (data) => { pendingMoof = data.slice(); },
				onMdat: (data) => {
					// Each moof + mdat pair is one media segment.
					if (cancelled() || !pendingMoof) return;
					const segment = concatBytes(pendingMoof, data.slice());
					pendingMoof = null;
					const isFirst = firstSegment === null;
					if (isFirst) firstSegment = segment;
					enqueue(segment, isFirst ? { onAppended: () => calibrateTimeline(gen, anchor.time, segment, () => calibrated, (v) => { calibrated = v; }) } : undefined);
				},
			}),
		});

		const current = { output, cancelled, gen };
		session = current;

		const videoCodec = await videoTrack.getCodec();
		const videoSource = new MB.EncodedVideoPacketSource(videoCodec);
		output.addVideoTrack(videoSource);

		let audioSource = null;
		if (audioTrack) {
			audioSource = new MB.EncodedAudioPacketSource(await audioTrack.getCodec());
			output.addAudioTrack(audioSource);
		}

		await output.start();
		if (cancelled()) { await output.cancel().catch(() => {}); return null; }

		// Pump packets in the background; the caller does not await this.
		pumpPackets({ output, videoSource, audioSource, startTime, anchor, cancelled }).catch((err) => {
			if (!cancelled()) fail(err);
		});

		return current;
	}

	async function pumpPackets({ output, videoSource, audioSource, startTime, anchor, cancelled }) {
		const videoSink = new MB.EncodedPacketSink(videoTrack);
		const audioSink = audioTrack ? new MB.EncodedPacketSink(audioTrack) : null;

		// Decoding can only begin at a keyframe, so a seek lands on the last one at
		// or before the target. The media element then skips the gap itself.
		let vPacket = startTime > 0
			? await videoSink.getKeyPacket(startTime)
			: await videoSink.getFirstPacket();
		if (!vPacket) throw new Error('No video packets at the requested position');
		// Recorded before the first add(), so it is always set by the time the muxer
		// emits a segment and calibrateTimeline runs.
		anchor.time = vPacket.timestamp;

		// Start audio at the video keyframe's timestamp so the tracks stay aligned
		// instead of the audio running ahead of the first decodable picture.
		let aPacket = audioSink ? await audioSink.getKeyPacket(vPacket.timestamp) : null;
		// Audio may begin later than the first video keyframe. A null predecessor
		// does not mean there is no audio; start at its first packet in that case.
		if (audioSink && !aPacket) aPacket = await audioSink.getFirstPacket();

		let firstVideo = true;
		let firstAudio = true;

		while (!cancelled() && (vPacket || aPacket)) {
			// Interleave by timestamp: always advance whichever track is further
			// behind, so the muxer can close fragments as it goes.
			const takeVideo = vPacket && (!aPacket || vPacket.timestamp <= aPacket.timestamp);
			if (takeVideo) {
				const meta = firstVideo ? { decoderConfig: await videoTrack.getDecoderConfig() } : undefined;
				await videoSource.add(vPacket, meta);
				firstVideo = false;
				vPacket = await videoSink.getNextPacket(vPacket);
			} else {
				const meta = firstAudio ? { decoderConfig: await audioTrack.getDecoderConfig() } : undefined;
				await audioSource.add(aPacket, meta);
				firstAudio = false;
				aPacket = await audioSink.getNextPacket(aPacket);
			}
			await waitForBufferRoom(cancelled);
		}

		if (cancelled()) return;
		await output.finalize();
		if (!cancelled()) {
			outputEnded = true;
			maybeEndStream(); // updateend will finish once the last append drains
		}
	}

	// Idle while the buffer is far enough ahead of the playhead. Without this the
	// pump would race to the end of the file and blow the SourceBuffer quota.
	function waitForBufferRoom(cancelled) {
		const hasRoom = () => !quotaBlocked && queuedBytes < MAX_QUEUED_BYTES &&
			bufferedAhead(video.buffered, video.currentTime) < BUFFER_AHEAD_SEC;
		if (cancelled() || hasRoom()) return Promise.resolve();
		return new Promise((resolve) => {
			const timer = setInterval(() => {
				if (cancelled() || hasRoom()) {
					clearInterval(timer);
					resolve();
				}
			}, 250);
		});
	}

	function onTimeUpdate() {
		if (destroyed) return;
		evictBehind(quotaBlocked);
		drainQueue();
	}

	// A fragmented MP4 written from a mid-file keyframe may carry either the source's
	// absolute timestamps or timestamps rebased to zero, depending on the muxer. We
	// cannot know which without looking, so after the first media segment of a
	// seeked session lands we check where it actually appeared on the timeline and,
	// if it was rebased, set timestampOffset and replay that segment.
	//
	// The comparison MUST be against the keyframe the session started from, not the
	// seek target: the muxer writes the keyframe's own timestamp, which sits up to a
	// GOP earlier. Measuring against the target instead shifts the whole rest of the
	// timeline forward by that gap (desyncing subtitles and the scrubber) and pins
	// the buffer's start exactly on currentTime, which Chrome refuses to treat as
	// seekable — the seek then never completes and playback stops dead.
	// Mediabunny 1.55 preserves absolute timestamps, so in practice this is a no-op.
	function calibrateTimeline(gen, anchorTime, segment, isCalibrated, setCalibrated) {
		if (destroyed || gen !== generation || isCalibrated() || !sourceBuffer) return;
		setCalibrated(true);

		const ranges = sourceBuffer.buffered;
		if (!ranges.length) return;
		const landedAt = ranges.start(ranges.length - 1);
		const drift = anchorTime - landedAt;
		if (Math.abs(drift) < 1) return; // timestamps were preserved; nothing to do

		try {
			sourceBuffer.timestampOffset = drift;
			sourceBuffer.remove(landedAt, Infinity);
			appendQueue.unshift({ bytes: segment });
			queuedBytes += segment.byteLength;
			drainQueue();
		} catch (err) {
			console.warn('[mkvplayer] timeline calibration failed', err);
		}
	}

	// --- seeking -----------------------------------------------------------

	async function restartAt(startTime) {
		outputEnded = false;
		const stale = session;
		session = null;
		generation++; // makes the old session's callbacks no-ops immediately
		if (stale) await stale.output.cancel().catch(() => {});

		appendQueue.length = 0;
		queuedBytes = 0;
		quotaBlocked = false;
		appending = false;
		if (destroyed) return;
		if (sourceBuffer) {
			try {
				if (sourceBuffer.updating) sourceBuffer.abort();
				sourceBuffer.timestampOffset = 0;
				sourceBuffer.remove(0, Infinity);
			} catch (err) { /* best-effort; the new session overwrites anyway */ }
		}
		await startSession(startTime);
	}

	let seekPending = false;
	let latestSeek = null;
	function onSeeking() {
		if (destroyed) return;
		const target = video.currentTime;
		if (!seekPending && isBuffered(video.buffered, target)) return;
		latestSeek = target;
		if (seekPending) return;
		seekPending = true;
		onStatus('Đang tua…');
		(async () => {
			while (!destroyed && latestSeek !== null) {
				const next = latestSeek;
				latestSeek = null;
				await restartAt(next);
			}
		})().catch(fail).finally(() => { seekPending = false; });
	}

	// --- wiring ------------------------------------------------------------

	function onSourceOpen() {
		mediaSource.removeEventListener('sourceopen', onSourceOpen);
		if (destroyed) return;
		try {
			if (Number.isFinite(duration) && duration > 0) mediaSource.duration = duration;
			sourceBuffer = mediaSource.addSourceBuffer(mimeType);
			sourceBuffer.mode = 'segments';
			sourceBuffer.addEventListener('updateend', onUpdateEnd);
			sourceBuffer.addEventListener('error', onBufferError);
		} catch (err) {
			fail(err);
			return;
		}
		startSession(0).catch(fail);
	}

	mediaSource.addEventListener('sourceopen', onSourceOpen);
	video.addEventListener('seeking', onSeeking);
	video.addEventListener('timeupdate', onTimeUpdate);
	signal?.addEventListener('abort', destroy, { once: true });
	video.src = objectURL;

	function onBufferError() {
		fail(new Error('MediaSource rejected a video segment'));
	}

	function destroy() {
		if (destroyed) return;
		destroyed = true;
		generation++;
		signal?.removeEventListener('abort', destroy);
		mediaSource.removeEventListener('sourceopen', onSourceOpen);
		video.removeEventListener('seeking', onSeeking);
		video.removeEventListener('timeupdate', onTimeUpdate);
		if (sourceBuffer) {
			sourceBuffer.removeEventListener('updateend', onUpdateEnd);
			sourceBuffer.removeEventListener('error', onBufferError);
		}
		if (session) session.output.cancel().catch(() => {});
		session = null;
		appendQueue.length = 0;
		queuedBytes = 0;
		try { if (mediaSource.readyState === 'open') mediaSource.endOfStream(); } catch (err) { /* ignore */ }
		URL.revokeObjectURL(objectURL);
		input.dispose();
	}
	return { destroy };
}
