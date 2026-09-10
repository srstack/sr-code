// usher SPA: embedded agent UIs — the sidebar "Interfaces" section and the
// #/ui/{name} full-view iframe route. Data comes from GET /api/embeds
// ([{name,title,port,ready,query?}]); each ready embed serves its own UI on
// 127.0.0.1:<port>/ behind the same auth cookie as the main UI.

import {
  esc, root, subtitle, closeES, clearListInterval,
  setCurrentDetailId, setCurrentDraftKey,
} from './state.js';

// --- embedview-private state ---
let embeds = [];           // latest /api/embeds payload
let lastSidebarHtml = '';  // innerHTML cache: skip rewrites when nothing changed
let lastViewHtml = '';     // same for the active #/ui/ view (an iframe rewrite
                           // would reload it, so the cache is load-bearing here)
let activeEmbed = null;    // embed name currently routed to; null off #/ui/
let startRetry = null;     // 5s refetch timer while the viewed embed is !ready

// ---------- data ----------

// Polled once at load and every 30s from app.js. Also re-renders the active
// #/ui/ view, so a poll that flips ready swaps the "Starting…" note for the
// iframe without a route change.
export async function loadEmbeds() {
  try {
    const res = await fetch('/api/embeds');
    embeds = res.ok ? (await res.json() || []) : [];
  } catch {/* server may be down briefly — keep the last list */}
  renderSidebarEmbeds();
  if (activeEmbed) renderEmbedView(activeEmbed);
}

// ---------- sidebar "Interfaces" section ----------

// One row per embed under the session list: green dot when the child is up,
// grey while it is still starting. The whole section stays hidden when no
// embeds are configured.
function renderSidebarEmbeds() {
  const wrap = document.getElementById('sidebar-interfaces');
  const list = document.getElementById('sidebar-embeds');
  if (!wrap || !list) return;
  wrap.hidden = embeds.length === 0;
  const html = embeds.map(e => {
    const href = '#/ui/' + encodeURIComponent(e.name);
    const dot = e.ready
      ? '<span class="running-dot" title="running">●</span>'
      : '<span class="running-dot starting" title="starting">●</span>';
    return `<li class="sidebar-item embed-row">
      <a href="${esc(href)}" data-route="ui:${esc(e.name)}" title="${esc(e.title)}">${dot}${esc(e.title)}</a>
    </li>`;
  }).join('');
  if (html === lastSidebarHtml) return;
  lastSidebarHtml = html;
  list.innerHTML = html;
}

// ---------- #/ui/{name} view ----------

// Full-view iframe with no usher header chrome (app.js tags the route on
// <body>; style.css hides the header). While the child is still starting, a
// centered note stands in and the next poll (or a 5s retry) swaps it out.
export function showEmbed(name) {
  clearListInterval();
  closeES();
  setCurrentDetailId(null);
  setCurrentDraftKey(null);
  const e = embeds.find(x => x.name === name);
  subtitle.textContent = e ? e.title : name;
  activeEmbed = name;
  lastViewHtml = ''; // force a fresh mount even if the markup matches a prior view
  renderEmbedView(name);
}

// Called by app.js on every route that is NOT #/ui/*, so the start-retry
// timer doesn't keep polling after the user navigates away.
export function leaveEmbed() {
  activeEmbed = null;
  if (startRetry) { clearTimeout(startRetry); startRetry = null; }
}

function renderEmbedView(name) {
  const e = embeds.find(x => x.name === name);
  let html;
  if (e && e.ready) {
    const src = location.protocol + '//' + location.hostname + ':' + e.port + '/' + (e.query || '');
    html = `<div class="embed-view"><iframe src="${esc(src)}" title="${esc(e.title)}"></iframe></div>`;
  } else if (e) {
    html = `<div class="embed-view"><div class="embed-note">Starting ${esc(e.title)}…</div></div>`;
  } else {
    html = `<div class="embed-view"><div class="embed-note">Unknown interface "${esc(name)}"</div></div>`;
  }
  if (html !== lastViewHtml) {
    lastViewHtml = html;
    root.innerHTML = html;
  }
  // While the viewed embed is up but not ready, poll faster than the 30s
  // baseline so the note swaps to the iframe soon after the child binds.
  if (startRetry) { clearTimeout(startRetry); startRetry = null; }
  if (e && !e.ready && activeEmbed === name) {
    startRetry = setTimeout(() => {
      startRetry = null;
      if (activeEmbed === name) loadEmbeds();
    }, 5000);
  }
}
