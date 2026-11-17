// Watch progress ("continue watching"): where each movie and episode was left
// off. It lives in this browser's localStorage only and never reaches the
// server, so it works signed out and does not follow an account to another
// device.
//
// The watch page (templates/watch.html) is the only writer. Everything else
// here only reads it:
//   - a thin bar on poster cards: the newest unfinished entry for that title,
//     so a series card shows how far into the current episode the visitor is;
//   - a bar on every [data-progress="<kind>:<id>"] element (the detail page's
//     episode rows, the watch page's episode chips), where a finished episode
//     shows a full bar;
//   - the "Continue watching" row on the browse home (#continue-row).
//
// Format, under phimnet.progress (same convention as phimnet.bookmarks): a
// newest-first JSON array, capped at MAX, of
//   { k: "movie:12" | "episode:345", title: <title id>, s: season, e: episode,
//     t: seconds reached, d: duration in seconds (0 = unknown), done: bool,
//     at: ms timestamp }
// Card metadata is deliberately NOT stored. The row fetches it localized from
// /api/catalog/cards, so a language switch or a renamed title cannot go stale.
//
// Loaded deferred from layout.html, before bookmarks.js, which calls decorate()
// on the cards it builds itself.
(function () {
	'use strict';

	var KEY = 'phimnet.progress';
	var MAX = 300;          // far under quota; the oldest entries fall off the end
	var MIN_SECONDS = 30;   // anything earlier is not worth resuming
	var DONE_RATIO = 0.95;  // past this the credits are rolling: count it watched
	var ROW_MAX = 20;
	var MSG = {};
	try { MSG = JSON.parse(document.body.dataset.clientMessages || '{}'); } catch (e) {}
	function msg(key, fallback) { return MSG[key] || fallback; }
	var LOCALE = document.body.dataset.locale || 'en';

	// Storage can throw (Safari private mode, quota, blocked site data). Every
	// access is guarded, so a blocked store degrades to "nothing watched".
	function load() {
		try {
			var list = JSON.parse(localStorage.getItem(KEY) || '[]');
			return Array.isArray(list) ? list.filter(function (e) { return e && typeof e.k === 'string'; }) : [];
		} catch (e) { return []; }
	}

	function store(list) {
		try { localStorage.setItem(KEY, JSON.stringify(list.slice(0, MAX))); } catch (e) {}
	}

	function keyOf(kind, id) { return kind + ':' + id; }

	function indexOf(list, k) {
		for (var i = 0; i < list.length; i++) {
			if (list[i].k === k) return i;
		}
		return -1;
	}

	function get(kind, id) {
		var list = load();
		var at = indexOf(list, keyOf(kind, id));
		return at >= 0 ? list[at] : null;
	}

	// resumable is the entry the watch page should offer to continue from, or null.
	function resumable(kind, id) {
		var e = get(kind, id);
		return e && !e.done && e.t >= MIN_SECONDS ? e : null;
	}

	// save records a position and moves the entry to the front. A duration the
	// player cannot tell (a live-transcoded stream reports Infinity) keeps the
	// one recorded earlier rather than erasing it.
	function save(kind, id, rec) {
		var k = keyOf(kind, id);
		var list = load();
		var at = indexOf(list, k);
		var prev = at >= 0 ? list[at] : null;
		if (at >= 0) list.splice(at, 1);
		var d = isFinite(rec.d) && rec.d > 0 ? Math.round(rec.d) : (prev && prev.d) || 0;
		var t = Math.max(0, Math.floor(rec.t || 0));
		var done = !!rec.done || (d > 0 && t >= d * DONE_RATIO);
		list.unshift({
			k: k, title: Number(rec.title) || 0, s: Number(rec.s) || 0, e: Number(rec.e) || 0,
			t: t, d: d, done: done, at: Date.now()
		});
		store(list);
	}

	function remove(kind, id) {
		var list = load();
		var at = indexOf(list, keyOf(kind, id));
		if (at < 0) return;
		list.splice(at, 1);
		store(list);
	}

	// dropUnfinished takes a title off the "Continue watching" row. Finished
	// entries stay, so the episode list keeps its watched marks.
	function dropUnfinished(titleID) {
		store(load().filter(function (e) { return String(e.title) !== String(titleID) || e.done; }));
	}

	function percent(e) {
		if (!e) return 0;
		if (e.done) return 100;
		if (!(e.d > 0)) return 0;
		return Math.max(2, Math.min(100, e.t / e.d * 100));
	}

	// formatTime renders seconds as m:ss, or h:mm:ss from an hour up.
	function formatTime(sec) {
		sec = Math.max(0, Math.floor(sec || 0));
		var h = Math.floor(sec / 3600);
		var m = Math.floor(sec % 3600 / 60);
		var s = sec % 60;
		var pad = function (n) { return n < 10 ? '0' + n : String(n); };
		return h ? h + ':' + pad(m) + ':' + pad(s) : m + ':' + pad(s);
	}

	function el(tag, cls, text) {
		var node = document.createElement(tag);
		if (cls) node.className = cls;
		if (text != null) node.textContent = text; // never innerHTML
		return node;
	}

	// setBar puts a progress bar at the end of host, replaces the one there, or
	// removes it when there is nothing to show.
	function setBar(host, pct, done) {
		if (!host) return;
		var bar = host.querySelector(':scope > .wp-bar');
		if (!(pct > 0)) { if (bar) bar.remove(); return; }
		if (!bar) {
			bar = el('div', 'wp-bar');
			bar.setAttribute('aria-hidden', 'true');
			bar.appendChild(el('span'));
			host.appendChild(bar);
		}
		bar.classList.toggle('is-done', !!done);
		bar.firstChild.style.width = pct.toFixed(1) + '%';
	}

	// unfinishedByTitle maps a title id to its newest unfinished entry.
	function unfinishedByTitle(list) {
		var out = {};
		list.forEach(function (e) {
			if (e.done || e.t < MIN_SECONDS || !e.title) return;
			if (!(e.title in out)) out[e.title] = e; // list is newest-first
		});
		return out;
	}

	// decorate draws the bars inside root (default: the whole page).
	function decorate(root) {
		root = root || document;
		var list = load();
		var byKey = {};
		list.forEach(function (e) { byKey[e.k] = e; });
		var byTitle = unfinishedByTitle(list);
		root.querySelectorAll('[data-progress]').forEach(function (host) {
			var e = byKey[host.dataset.progress];
			setBar(host, percent(e), e && e.done);
		});
		root.querySelectorAll('.card').forEach(function (card) {
			if (card.closest('#continue-row')) return; // those carry their own bar
			var btn = card.querySelector('[data-bm-id]');
			if (!btn) return;
			setBar(card.querySelector('.poster'), percent(byTitle[btn.dataset.bmId]), false);
		});
	}

	// --- Continue watching row (browse home) ---------------------------------

	function watchHref(e) {
		var kind = e.k.split(':')[0];
		var id = e.k.split(':')[1];
		return '/' + LOCALE + '/watch/' + (kind === 'episode' ? 'episode' : 'movie') + '/' + encodeURIComponent(id);
	}

	// format fills %s / %d in order, like the watch page's i18nFormat.
	function format(value) {
		var args = Array.prototype.slice.call(arguments, 1);
		var i = 0;
		return String(value).replace(/%[sd]/g, function () {
			var v = args[i++];
			return v == null ? '' : String(v);
		});
	}

	// buildContinueCard reuses the "card" partial's classes (templates/grid.html)
	// so it shares the stylesheet, but links to the watch page rather than the
	// title, and swaps the save button for a remove one.
	function buildContinueCard(e, card) {
		var node = el('div', 'card card-continue');
		var poster = el('div', 'poster');
		if (card.poster) {
			var img = el('img');
			img.src = card.poster;
			img.alt = card.title;
			img.loading = 'lazy';
			poster.appendChild(img);
		} else {
			poster.appendChild(el('div', 'poster-empty', card.title));
		}
		var play = el('span', 'card-play', '▶');
		play.setAttribute('aria-hidden', 'true');
		poster.appendChild(play);
		var rm = el('button', 'card-remove', '✕');
		rm.type = 'button';
		rm.dataset.continueRemove = String(e.title);
		rm.setAttribute('aria-label', msg('continueRemove', 'Remove from Continue watching'));
		rm.title = rm.getAttribute('aria-label');
		poster.appendChild(rm);
		setBar(poster, percent(e), false);
		node.appendChild(poster);

		var body = el('div', 'card-body');
		var title = el('div', 'card-title');
		var link = el('a', 'card-link', card.title);
		link.href = watchHref(e);
		title.appendChild(link);
		body.appendChild(title);
		if (e.k.indexOf('episode:') === 0 && e.s && e.e) {
			body.appendChild(el('div', 'card-meta', format(msg('episodeLabel', 'Season %d · Episode %d'), e.s, e.e)));
		}
		if (e.d > 0) {
			var left = Math.max(1, Math.ceil((e.d - e.t) / 60));
			body.appendChild(el('div', 'card-meta', format(msg('continueRemaining', '%d min left'), left)));
		}
		node.appendChild(body);
		return node;
	}

	// bindArrows mirrors the carousel script in templates/rows.html, which runs
	// before this row has any cards and so never sees it.
	function bindArrows(rail) {
		var track = rail.querySelector('.row-track');
		var prev = rail.querySelector('.row-arrow-prev');
		var next = rail.querySelector('.row-arrow-next');
		if (!track || !prev || !next) return;
		function update() {
			var max = track.scrollWidth - track.clientWidth - 1;
			prev.hidden = track.scrollLeft <= 0;
			next.hidden = track.scrollLeft >= max;
		}
		function step(dir) { track.scrollBy({ left: dir * track.clientWidth * 0.9, behavior: 'smooth' }); }
		prev.addEventListener('click', function () { step(-1); });
		next.addEventListener('click', function () { step(1); });
		track.addEventListener('scroll', update, { passive: true });
		window.addEventListener('resize', update);
		update();
	}

	var rowBound = false;

	function renderContinueRow() {
		var row = document.getElementById('continue-row');
		if (!row) return;
		// One card per title: its newest unfinished entry, newest title first.
		var list = load();
		var byTitle = unfinishedByTitle(list);
		var items = list.filter(function (e) { return byTitle[e.title] === e; }).slice(0, ROW_MAX);
		if (!items.length) { row.hidden = true; return; }
		var ids = items.map(function (e) { return e.title; });
		fetch('/api/catalog/cards?locale=' + encodeURIComponent(LOCALE) + '&ids=' + encodeURIComponent(ids.join(',')))
			.then(function (res) { if (!res.ok) throw new Error('HTTP ' + res.status); return res.json(); })
			.then(function (data) {
				var cards = {};
				(data.cards || []).forEach(function (c) { cards[String(c.id)] = c; });
				var track = row.querySelector('.row-track');
				track.textContent = '';
				items.forEach(function (e) {
					var c = cards[String(e.title)];
					if (c) track.appendChild(buildContinueCard(e, c));
				});
				row.hidden = !track.children.length;
				if (!row.hidden && !rowBound) { rowBound = true; bindArrows(row.querySelector('.row-rail')); }
			})
			.catch(function () { row.hidden = true; }); // no metadata, nothing to show
	}

	document.addEventListener('click', function (ev) {
		var btn = ev.target.closest('[data-continue-remove]');
		if (!btn) return;
		// The card is covered by a stretched .card-link overlay; stop the click
		// before it navigates to the watch page.
		ev.preventDefault();
		ev.stopPropagation();
		dropUnfinished(btn.dataset.continueRemove);
		var card = btn.closest('.card');
		var track = card && card.parentNode;
		if (card) card.remove();
		var row = document.getElementById('continue-row');
		if (row && track && !track.children.length) row.hidden = true;
		decorate();
	});

	// Another tab finishing an episode should show up here without a reload.
	window.addEventListener('storage', function (ev) {
		if (ev.key !== null && ev.key !== KEY) return;
		decorate();
		renderContinueRow();
	});

	window.phimnetProgress = {
		MIN_SECONDS: MIN_SECONDS,
		get: get,
		resumable: resumable,
		save: save,
		remove: remove,
		percent: percent,
		formatTime: formatTime,
		decorate: decorate
	};

	decorate();
	renderContinueRow();
})();
