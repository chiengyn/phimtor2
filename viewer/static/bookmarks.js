// Bookmarks ("xem sau"): the watch-later list, in one of two modes.
//
//   local  — anonymous visitors. The list lives entirely in localStorage and each
//            entry stores the whole card snapshot it was saved from, which is
//            what lets /bookmarks re-render it with no round trip. This is
//            exactly how the feature worked before accounts existed, and nothing
//            about it changed.
//   server — signed-in visitors. The list lives in the database, so it follows
//            them across devices. Only ids are held here (the set the server
//            rendered onto <body data-bm-ids>); the cards themselves come from
//            the server-rendered "card" partial, so they can never go stale.
//
// On any load where a signed-in visitor still has a localStorage list, it is
// merged into their account and then dropped. That is deliberately not gated on
// a "just logged in" marker, so it self-heals on every device they use.
//
// Loaded from layout.html on every page, so any rendered card (home rows, the
// filtered grid, the detail page, the bookmarks page itself) gets a working save
// button. All wiring is delegated off document, so cards this script creates need
// no extra binding.
(function () {
	'use strict';

	var KEY = 'phimnet.bookmarks';
	var MAX = 500; // far under quota; oldest entries fall off the end

	var SERVER = document.body.dataset.bmMode === 'server';
	// Ids the signed-in visitor has saved, as strings so they compare cleanly
	// against the data-bm-id attributes. Unused in local mode.
	var savedIDs = new Set();
	if (SERVER) {
		try {
			(JSON.parse(document.body.dataset.bmIds || '[]') || []).forEach(function (id) {
				savedIDs.add(String(id));
			});
		} catch (e) {}
	}

	// api wraps fetch for the saved-titles endpoints. same-origin credentials so
	// the session cookie rides along; any non-2xx rejects so callers can revert.
	function api(path, method, body) {
		return fetch(path, {
			method: method,
			credentials: 'same-origin',
			headers: body ? { 'Content-Type': 'application/json' } : undefined,
			body: body ? JSON.stringify(body) : undefined
		}).then(function (res) {
			if (!res.ok) throw new Error('HTTP ' + res.status);
			return res.json().catch(function () { return {}; });
		});
	}

	// Storage can throw (Safari private mode, quota, disabled cookies). Every
	// access is guarded so a blocked store degrades to "nothing saved" instead of
	// breaking the page. Same convention as the watch page's subtitle prefs.
	function load() {
		try {
			var list = JSON.parse(localStorage.getItem(KEY) || '[]');
			return Array.isArray(list) ? list : [];
		} catch (e) { return []; }
	}

	function save(list) {
		try { localStorage.setItem(KEY, JSON.stringify(list.slice(0, MAX))); } catch (e) {}
	}

	function dropLocal() {
		try { localStorage.removeItem(KEY); } catch (e) {}
	}

	// isSaved / savedCount are the two questions the UI asks, answered from
	// whichever store this page is using.
	function isSaved(id) {
		if (SERVER) return savedIDs.has(String(id));
		return indexOf(load(), id) >= 0;
	}

	function savedCount() {
		return SERVER ? savedIDs.size : load().length;
	}

	function indexOf(list, id) {
		for (var i = 0; i < list.length; i++) {
			if (String(list[i].id) === String(id)) return i;
		}
		return -1;
	}

	// snapshotFrom reads the data-bm-* payload the templates render onto every
	// save button (see templates/grid.html and detail.html).
	function snapshotFrom(btn) {
		var d = btn.dataset;
		return {
			id: d.bmId,
			href: d.bmHref,
			title: d.bmTitle || '',
			original: d.bmOriginal || '',
			poster: d.bmPoster || '',
			year: d.bmYear || '',
			type: d.bmType || 'movie',
			score: parseFloat(d.bmScore) || 0,
			vietsub: d.bmVietsub === 'true',
			savedAt: Date.now()
		};
	}

	// toggle returns whether the title is saved afterwards. New saves go to the
	// front so the list reads newest-first.
	function toggle(entry) {
		var list = load();
		var at = indexOf(list, entry.id);
		if (at >= 0) {
			list.splice(at, 1);
			save(list);
			return false;
		}
		list.unshift(entry);
		save(list);
		return true;
	}

	function markButton(btn, saved) {
		btn.classList.toggle('is-saved', saved);
		btn.setAttribute('aria-pressed', saved ? 'true' : 'false');
		btn.setAttribute('aria-label', saved ? 'Bỏ lưu' : 'Lưu xem sau');
		var label = btn.querySelector('.bm-label');
		if (label) label.textContent = saved ? 'Đã lưu' : 'Lưu xem sau';
	}

	function syncButtons() {
		document.querySelectorAll('[data-bm-toggle]').forEach(function (btn) {
			markButton(btn, isSaved(btn.dataset.bmId));
		});
	}

	function syncCount() {
		var n = savedCount();
		document.querySelectorAll('.saved-count').forEach(function (el) {
			el.textContent = n;
			el.hidden = n === 0;
		});
	}

	function el(tag, cls, text) {
		var node = document.createElement(tag);
		if (cls) node.className = cls;
		if (text != null) node.textContent = text; // never innerHTML: values come from storage
		return node;
	}

	// buildCard mirrors the "card" partial in templates/grid.html so the saved
	// list reuses the exact same stylesheet. Keep the two in sync.
	function buildCard(entry) {
		var card = el('div', 'card');

		var poster = el('div', 'poster');
		if (entry.poster) {
			var img = el('img');
			img.src = entry.poster;
			img.alt = entry.title;
			img.loading = 'lazy';
			poster.appendChild(img);
		} else {
			poster.appendChild(el('div', 'poster-empty', entry.title));
		}
		var play = el('span', 'card-play', '▶');
		play.setAttribute('aria-hidden', 'true');
		poster.appendChild(play);
		if (entry.score > 0) {
			var score = el('span', 'card-score', entry.score.toFixed(1));
			score.setAttribute('aria-hidden', 'true');
			poster.appendChild(score);
		}
		if (entry.vietsub) {
			var vs = el('span', 'card-vietsub', 'Vietsub');
			vs.setAttribute('aria-hidden', 'true');
			poster.appendChild(vs);
		}

		var btn = el('button', 'card-bookmark');
		btn.type = 'button';
		btn.setAttribute('data-bm-toggle', '');
		btn.dataset.bmId = entry.id;
		btn.dataset.bmHref = entry.href;
		btn.dataset.bmTitle = entry.title;
		btn.dataset.bmOriginal = entry.original;
		btn.dataset.bmPoster = entry.poster;
		btn.dataset.bmYear = entry.year;
		btn.dataset.bmType = entry.type;
		btn.dataset.bmScore = entry.score;
		btn.dataset.bmVietsub = entry.vietsub ? 'true' : 'false';
		var icon = el('span', 'bm-icon');
		icon.setAttribute('aria-hidden', 'true');
		btn.appendChild(icon);
		markButton(btn, true);
		poster.appendChild(btn);
		card.appendChild(poster);

		var body = el('div', 'card-body');
		var title = el('div', 'card-title');
		var link = el('a', 'card-link', entry.title);
		link.href = entry.href;
		title.appendChild(link);
		body.appendChild(title);
		if (entry.original && entry.original !== entry.title) {
			body.appendChild(el('div', 'card-original', entry.original));
		}
		var kind = entry.type === 'tv' ? 'Phim bộ' : 'Phim lẻ';
		body.appendChild(el('div', 'card-meta', entry.year ? entry.year + ' · ' + kind : kind));
		card.appendChild(body);

		return card;
	}

	// syncEmptyState shows/hides the "nothing saved yet" copy and the clear
	// button from whatever is currently in the grid. Used by both modes.
	function syncEmptyState() {
		var grid = document.getElementById('bookmark-grid');
		if (!grid) return;
		var n = grid.querySelectorAll('.card').length;
		var empty = document.getElementById('bookmark-empty');
		var clear = document.getElementById('bookmark-clear');
		if (empty) empty.hidden = n > 0;
		if (clear) clear.hidden = n === 0;
	}

	// renderBookmarks rebuilds the saved grid from localStorage. Local mode only:
	// in server mode the grid arrives server-rendered by the same "card" partial
	// the discovery grid uses, and this script only ever removes nodes from it.
	function renderBookmarks() {
		var grid = document.getElementById('bookmark-grid');
		if (!grid) return; // not the bookmarks page
		if (SERVER) {
			syncEmptyState();
			return;
		}
		var list = load();
		grid.textContent = '';
		list.forEach(function (entry) { grid.appendChild(buildCard(entry)); });
		syncEmptyState();
	}

	// dropCard removes an un-saved title from the saved grid, if we are on it.
	function dropCard(btn) {
		if (!document.getElementById('bookmark-grid')) return;
		var card = btn.closest('.card');
		if (card) card.remove();
		syncEmptyState();
	}

	// One delegated handler covers every save button on the page, including the
	// cards buildCard creates above.
	document.addEventListener('click', function (ev) {
		var btn = ev.target.closest('[data-bm-toggle]');
		if (!btn) return;
		// The card is covered by a stretched .card-link overlay; stop the click
		// before it navigates to the title.
		ev.preventDefault();
		ev.stopPropagation();

		if (!SERVER) {
			var nowSaved = toggle(snapshotFrom(btn));
			markButton(btn, nowSaved);
			syncCount();
			// On the bookmarks page an un-saved card leaves the list immediately.
			if (!nowSaved) renderBookmarks();
			return;
		}

		// Server mode: mark optimistically so the button feels instant, then
		// revert if the write fails. The card itself is only removed once the
		// server has confirmed, so there is nothing to rebuild on failure.
		var id = String(btn.dataset.bmId);
		var was = savedIDs.has(id);
		var want = !was;
		if (want) { savedIDs.add(id); } else { savedIDs.delete(id); }
		markButton(btn, want);
		syncCount();

		api('/api/bookmarks/' + encodeURIComponent(id), want ? 'POST' : 'DELETE')
			.then(function () {
				if (!want) dropCard(btn);
			})
			.catch(function () {
				if (was) { savedIDs.add(id); } else { savedIDs.delete(id); }
				markButton(btn, was);
				syncCount();
			});
	});

	document.addEventListener('click', function (ev) {
		if (!ev.target.closest('#bookmark-clear')) return;
		if (!window.confirm('Xoá toàn bộ danh sách phim đã lưu?')) return;
		if (!SERVER) {
			save([]);
			renderBookmarks();
			syncButtons();
			syncCount();
			return;
		}
		api('/api/bookmarks', 'DELETE').then(function () {
			savedIDs.clear();
			var grid = document.getElementById('bookmark-grid');
			if (grid) grid.textContent = '';
			syncEmptyState();
			syncButtons();
			syncCount();
		});
	});

	// Keep other tabs of the same site consistent. Local mode only — in server
	// mode localStorage is not the source of truth (and has been emptied).
	if (!SERVER) {
		window.addEventListener('storage', function (ev) {
			if (ev.key !== null && ev.key !== KEY) return;
			syncButtons();
			syncCount();
			renderBookmarks();
		});
	}

	// mergeLocal folds a pre-login localStorage list into the account. It runs on
	// any load where a signed-in visitor still has one, so it self-heals across
	// devices with no "just logged in" marker. The server merge is idempotent and
	// ignores ids that no longer exist, so a stale list is safe input.
	//
	// localStorage is only dropped once the server has accepted the list, so a
	// failed merge simply retries on the next page load.
	function mergeLocal() {
		var list = load();
		if (!list.length) return;
		var ids = [];
		list.forEach(function (entry) {
			var n = parseInt(entry.id, 10);
			if (n > 0) ids.push(n);
		});
		if (!ids.length) {
			dropLocal();
			return;
		}
		api('/api/bookmarks/merge', 'POST', { ids: ids }).then(function (data) {
			dropLocal();
			savedIDs.clear();
			(data.ids || []).forEach(function (id) { savedIDs.add(String(id)); });
			syncButtons();
			syncCount();
			// The saved grid was rendered before the merge, so it is now stale.
			if (document.getElementById('bookmark-grid')) window.location.reload();
		}).catch(function () {});
	}

	syncButtons();
	syncCount();
	renderBookmarks();
	if (SERVER) mergeLocal();
})();
