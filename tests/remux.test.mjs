// Run from any directory: node --test tests/remux.test.mjs
// Models asynchronous MSE operations; codec/container integration is tested by
// streamer/transcode_test.go with real FFmpeg. No npm dependencies are needed.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

const read = path => readFileSync(new URL('../' + path, import.meta.url), 'utf8');
const source = read('viewer/static/mkvplayer.js');
const tick = () => new Promise(resolve => setImmediate(resolve));
const ranges = { length: 0, start() { return 0; }, end() { return 0; } };
class Video extends EventTarget {
	isConnected = true;
	currentTime = 0;
	buffered = ranges;
	src = '';
	play() { return Promise.resolve(); }
	load() {}
	querySelectorAll() { return []; }
}
function harness(options = {}) {
	let disposed = 0, ended = 0, added = 0, audioAdded = 0;
	const seeks = [], errors = [], pendingUpdates = [];
	let ms;
	class Buffer extends EventTarget {
		updating = false;
		buffered = ranges;
		appendBuffer() {
			if (options.quota) throw new DOMException('full', 'QuotaExceededError');
			assert.equal(this.updating, false, 'append operations must be serialized');
			this.updating = true;
			const complete = () => { this.updating = false; this.dispatchEvent(new Event('updateend')); };
			if (options.holdAppends) pendingUpdates.push(complete); else setImmediate(complete);
		}
		remove() { this.appendBuffer(); }
		abort() { this.updating = false; }
	}
	class MediaSource extends EventTarget {
		static isTypeSupported() { return options.supported !== false; }
		readyState = 'open';
		buffer = new Buffer();
		constructor() { super(); ms = this; }
		addSourceBuffer() { if (options.bufferError) throw new Error('buffer failure'); return this.buffer; }
		endOfStream() { assert.equal(this.buffer.updating, false); this.readyState = 'ended'; ended++; }
	}
	const track = {
		getCodec: async () => 'avc', getCodecParameterString: async () => 'avc1.64001f',
		getDecoderConfig: async () => ({}),
		// Pure remux must work without the WebCodecs API.
		canDecode() { throw new Error('WebCodecs is not the MSE capability check'); },
	};
	const audioTrack = { ...track, audio: true };
	class Input {
		async getPrimaryVideoTrack() { return options.probe ? await options.probe : track; }
		async getPrimaryAudioTrack() { return options.audio ? audioTrack : null; }
		async computeDuration() { return 100; }
		async getFormat() { return { name: 'Matroska' }; }
		dispose() { disposed++; }
	}
	class PacketSink {
		constructor(track) { this.track = track; }
		async getFirstPacket() { if (options.pumpError) throw new Error('packet failure'); return { timestamp: this.track.audio ? 1 : 0 }; }
		async getKeyPacket(t) { if (this.track.audio) return null; seeks.push(t); return { timestamp: t }; }
		async getNextPacket(p) { return !this.track.audio && p.timestamp + 1 < (options.packets || 1) ? { timestamp: p.timestamp + 1 } : null; }
	}
	class PacketSource {
		async add() {
			if (this.audio) { audioAdded++; return; }
			added++;
			this.output.format.onMoof(new Uint8Array(8));
			this.output.format.onMdat(new Uint8Array(options.fragmentBytes || 8));
		}
	}
	class Output {
		constructor({ format }) { this.format = format; }
		addVideoTrack(s) { s.output = this; }
		addAudioTrack(s) { s.output = this; s.audio = true; }
		async start() { this.format.onFtyp(new Uint8Array(8)); this.format.onMoov(new Uint8Array(8)); }
		async cancel() { if (options.cancelWait) await options.cancelWait; }
		async finalize() {}
	}
	const MB = { Input, Output, UrlSource: class {}, ALL_FORMATS: [],
		StreamTarget: class {}, Mp4OutputFormat: class { constructor(opts) { Object.assign(this, opts); } },
		EncodedPacketSink: PacketSink, EncodedVideoPacketSource: PacketSource, EncodedAudioPacketSource: PacketSource };
	const context = vm.createContext({ MB, window: { MediaSource }, MediaSource,
		URL: { createObjectURL: () => 'blob:test', revokeObjectURL() {} },
		DOMException, WritableStream, Uint8Array, console, setInterval, clearInterval });
	vm.runInContext(source.replaceAll('export ', '').replace('import(MEDIABUNNY_URL)', 'Promise.resolve(MB)') + '\nglobalThis.attach = attachRemuxedSource;', context);
	return {
		video: new Video(), errors, options, pendingUpdates, seeks,
		get ms() { return ms; }, get disposed() { return disposed; }, get ended() { return ended; },
		get added() { return added; }, get audioAdded() { return audioAdded; },
		async attach(opts = {}) { return context.attach(this.video, 'https://stream/file?raw=1', { onError: err => errors.push(err), ...opts }); },
		async open() { ms.dispatchEvent(new Event('sourceopen')); await tick(); },
	};
}

test('admin and viewer remux implementations stay identical', () => {
	assert.equal(source, read('admin/static/mkvplayer.js'));
});
test('unsupported MSE codecs release the probe input without changing src', async () => {
	const h = harness({ supported: false });
	await assert.rejects(h.attach(), /will not accept/);
	assert.equal(h.disposed, 1);
	assert.equal(h.video.src, '');
});
test('cancelled probe never attaches a stale source', async () => {
	let resolve;
	const h = harness({ probe: new Promise(r => { resolve = r; }) });
	const abort = new AbortController();
	const result = h.attach({ signal: abort.signal });
	await tick(); abort.abort(); resolve(null);
	await assert.rejects(result);
	assert.ok(h.disposed >= 1);
	assert.equal(h.video.src, '');
});
test('endOfStream waits for the final queued append', async () => {
	const h = harness({ holdAppends: true });
	const controller = await h.attach();
	await h.open();
	assert.equal(h.ended, 0);
	while (h.pendingUpdates.length) h.pendingUpdates.shift()();
	assert.equal(h.ended, 1);
	assert.equal(h.errors.length, 0);
	controller.destroy();
});
for (const option of ['bufferError', 'pumpError']) {
	test(`${option} is reported once and releases resources`, async () => {
		const h = harness({ [option]: true });
		const controller = await h.attach();
		await h.open();
		assert.equal(h.errors.length, 1);
		assert.equal(h.disposed, 1);
		controller.destroy();
		assert.equal(h.disposed, 1);
	});
}
test('asynchronous SourceBuffer errors trigger fallback', async () => {
	const h = harness({ holdAppends: true });
	await h.attach(); await h.open();
	h.ms.buffer.dispatchEvent(new Event('error'));
	h.ms.buffer.dispatchEvent(new Event('error'));
	assert.equal(h.errors.length, 1);
	assert.equal(h.disposed, 1);
});
test('slow SourceBuffer applies byte-based backpressure', async () => {
	const h = harness({ holdAppends: true, packets: 100, fragmentBytes: 1024 * 1024 });
	const controller = await h.attach(); await h.open();
	assert.ok(h.added <= 8, `pump queued ${h.added} MiB without waiting`);
	assert.ok(h.added > 0);
	controller.destroy();
});
test('quota failure pauses the pump and timeupdate retries the append', async () => {
	const h = harness({ quota: true, packets: 100 });
	const controller = await h.attach(); await h.open();
	assert.ok(h.added <= 1);
	h.options.quota = false;
	h.video.dispatchEvent(new Event('timeupdate'));
	await new Promise(r => setTimeout(r, 280));
	assert.ok(h.added > 1);
	controller.destroy();
});
test('audio starting after the video keyframe is retained', async () => {
	const h = harness({ audio: true });
	const controller = await h.attach(); await h.open();
	assert.equal(h.audioAdded, 1);
	controller.destroy();
});
test('rapid seeks preserve the latest requested position', async () => {
	let release;
	const h = harness({ holdAppends: true, cancelWait: new Promise(r => { release = r; }) });
	const controller = await h.attach(); await h.open();
	h.video.currentTime = 20; h.video.dispatchEvent(new Event('seeking'));
	h.video.currentTime = 60; h.video.dispatchEvent(new Event('seeking'));
	release(); await tick(); await tick();
	assert.equal(h.seeks.at(-1), 60);
	controller.destroy();
});

test('admin native codec errors force transcode once; destroy removes listener', async () => {
	const context = vm.createContext({ URL, AbortController, document: { baseURI: 'https://admin/' }, console });
	vm.runInContext(read('admin/static/mkvattach.js').replace(/^import .*;$/m, '').replaceAll('export ', '') + '\nglobalThis.attach = attachSource;', context);
	const video = new Video();
	const controller = await context.attach(video, 'https://stream/file?token=x', 'file.webm');
	video.error = { code: 3 }; video.dispatchEvent(new Event('error'));
	assert.equal(video.src, 'https://stream/file?token=x&transcode=1');
	controller.destroy();
	video.src = 'new-source'; video.dispatchEvent(new Event('error'));
	assert.equal(video.src, 'new-source');
});

test('viewer watch script parses and fallback preserves query parameters', () => {
	const template = read('viewer/templates/watch.html');
	const script = template.match(/<script>([\s\S]*?)<\/script>/)[1];
	new vm.Script(script); // Inline JavaScript has no Go template interpolation.
	let aborted = 0, destroyed = 0;
	const context = vm.createContext({ URL, document: {
		baseURI: 'https://viewer/', createElement: () => ({}),
	}, fellBackToTranscode: false, remuxAbort: { abort() { aborted++; } },
	remuxController: { destroy() { destroyed++; } }, setStatus() {} });
	vm.runInContext(script.slice(script.indexOf('const NATIVE_CONTAINERS'), script.indexOf('// attachClientRemux')), context);
	const video = new Video();
	video.removeAttribute = () => {};
	video.appendChild = source => { video.src = source.src; };
	context.fallbackToTranscode(video, 'https://stream/file?token=x&raw=1');
	assert.equal(video.src, 'https://stream/file?token=x&transcode=1');
	context.fallbackToTranscode(video, 'https://stream/other');
	assert.equal(aborted, 1);
	assert.equal(destroyed, 1);
	assert.equal(video.src, 'https://stream/file?token=x&transcode=1');
});
