// Re-renders the server's <time datetime=… data-fmt=…> stamps (the `localtime`
// template func in server.go) in the browser's own time zone, keeping the Go
// layout the template asked for. Only the layout tokens the admin templates use
// are translated: 2006 01 02 15 04 05.
(() => {
  const pad = (n) => String(n).padStart(2, '0');
  const tokens = {
    '2006': (d) => String(d.getFullYear()),
    '01': (d) => pad(d.getMonth() + 1),
    '02': (d) => pad(d.getDate()),
    '15': (d) => pad(d.getHours()),
    '04': (d) => pad(d.getMinutes()),
    '05': (d) => pad(d.getSeconds()),
  };
  const re = /2006|01|02|15|04|05/g;
  const full = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'full', timeStyle: 'long',
  });

  function localize(root) {
    root.querySelectorAll('time[data-fmt]').forEach((el) => {
      const d = new Date(el.getAttribute('datetime'));
      if (isNaN(d)) return;
      el.textContent = el.dataset.fmt.replace(re, (tok) => tokens[tok](d));
      el.title = full.format(d);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => localize(document));
  } else {
    localize(document);
  }
  // htmx swaps (the crawl status re-polls every few seconds) bring new stamps.
  document.addEventListener('htmx:load', (e) => localize(e.target));
})();
