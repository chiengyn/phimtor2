// Bookmarks ("xem sau"): a per-browser watch-later list kept entirely in
// localStorage. The viewer is a public, account-less, read-only service, so there
// is no server side to this — each entry stores the whole card snapshot it was
// saved from, which is what lets /bookmarks re-render the list with no round trip.
//
// Loaded from layout.html on every page, so any rendered card (home rows, the
// filtered grid, the detail page, the bookmarks page itself) gets a working save
// button. All wiring is delegated off document, so cards this script creates need
// no extra binding.
(function () {
	'use strict';

	var KEY = 'phimnet.bookmarks';
	var MAX = 500; // far under quota; oldest entries fall off the end

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
		var list = load();
		document.querySelectorAll('[data-bm-toggle]').forEach(function (btn) {
			markButton(btn, indexOf(list, btn.dataset.bmId) >= 0);
		});
	}

	function syncCount() {
		var n = load().length;
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

	function renderBookmarks() {
		var grid = document.getElementById('bookmark-grid');
		if (!grid) return; // not the bookmarks page
		var empty = document.getElementById('bookmark-empty');
		var clear = document.getElementById('bookmark-clear');
		var list = load();

		grid.textContent = '';
		list.forEach(function (entry) { grid.appendChild(buildCard(entry)); });
		if (empty) empty.hidden = list.length > 0;
		if (clear) clear.hidden = list.length === 0;
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

		var saved = toggle(snapshotFrom(btn));
		markButton(btn, saved);
		syncCount();
		// On the bookmarks page an un-saved card leaves the list immediately.
		if (!saved && document.getElementById('bookmark-grid')) renderBookmarks();
	});

	document.addEventListener('click', function (ev) {
		if (!ev.target.closest('#bookmark-clear')) return;
		if (!window.confirm('Xoá toàn bộ danh sách phim đã lưu?')) return;
		save([]);
		renderBookmarks();
		syncButtons();
		syncCount();
	});

	// Keep other tabs of the same site consistent.
	window.addEventListener('storage', function (ev) {
		if (ev.key !== null && ev.key !== KEY) return;
		syncButtons();
		syncCount();
		renderBookmarks();
	});

	syncButtons();
	syncCount();
	renderBookmarks();
})();
