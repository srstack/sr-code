// usher SPA: hash-based routing between session list and detail view.
// Detail view streams subprocess events via SSE.

// Wrap fetch so any 401 from the API navigates to /login. SSE can't see
// HTTP status, so we rely on the polling fetches (session list, etc.) to
// trip this path within a few seconds of a cookie going bad.
const _origFetch = window.fetch.bind(window);
window.fetch = async (...args) => {
  const res = await _origFetch(...args);
  if (res.status === 401) {
    const next = encodeURIComponent(location.pathname + location.search + location.hash);
    location.href = '/login?next=' + next;
    // Return a never-resolving promise so callers don't proceed with the
    // 401 body. Navigation is about to take over the page anyway.
    return new Promise(() => {});
  }
  return res;
};

const root = document.getElementById('root');
const subtitle = document.getElementById('subtitle');
const renderModeBtn = document.getElementById('render-mode-btn');

let listInterval = null;
let currentES = null;
// The terminal-mirror SSE (the inline pane panel), tracked separately from
// currentES so the detail view's /events stream and the mirror's /screen
// stream can be open at once and both get torn down on navigation.
let currentScreenES = null;
// Detail-view transcript sync: the SSE renders only turns seen from
// subprocess.started, so we re-fetch on turn-end and on reconnect to catch the
// rest. Flags: detailStreaming (gate the reconnect re-fetch off a live turn),
// lastTranscriptSig (skip an unchanged rebuild), currentDetailId (ignore a
// re-fetch that resolves after the user navigated away).
let detailStreaming = false;
let lastTranscriptSig = '';
let currentDetailId = null;
// The committed transcript turns currently in the DOM, as {key, node} in order.
// Lets loadTranscript reconcile incrementally (append only what's new) instead
// of wiping and re-rendering all ~100 turns on every turn-end — the latter is
// O(n) per turn and visibly janks long, tool-heavy sessions. Tracking the nodes
// explicitly (rather than re-querying by position) keeps the diff correct even
// when untracked client-only bubbles — errors, optimistic placeholders — sit in
// the same list.
let renderedTurns = [];
// Transcript window: render the most recent `transcriptLimit` turns; "load
// earlier" grows it by a page and re-fetches. transcriptTotal is the server's
// full turn count (X-Transcript-Total), used to show/hide the button.
const TRANSCRIPT_PAGE = 100;
let transcriptLimit = TRANSCRIPT_PAGE;
let transcriptTotal = 0;
// Auto-scroll to the bottom on new content only when the user is already near
// it, so scrolling up to read history isn't yanked back down.
const BOTTOM_THRESHOLD = 64;
function isNearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < BOTTOM_THRESHOLD; }
// Set while a batch reconcile appends many turns so appendChatMessage skips its
// per-append scroll — reading scrollHeight each iteration forces a synchronous
// layout, making a big batch O(n²). The batch caller scrolls once at the end.
let suppressAppendScroll = false;
// Last sidebar HTML written to the DOM. The sidebar re-polls every 5s; skipping
// the innerHTML rewrite when nothing changed keeps the live-dot CSS animation
// from restarting (jumping back to its bright peak) on every poll.
let lastSidebarHtml = '';
// Last list-view rows HTML — same skip-when-unchanged trick as the sidebar,
// so the 5s poll doesn't restart status-dot animations or fight the filter.
let lastListRowsHtml = '';
let lastCwdSig = ''; // distinct-cwd set the list's cwd <select> was built from
let renderMode = localStorage.getItem('usher.renderMode') === 'raw' ? 'raw' : 'md';
// Per-cwd archived-disclosure expansion state. Session-only — refresh
// collapses everything, matching the assumption that browsing archived
// sessions is a rare action.
const cwdExpanded = new Set();

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
}

// renderMarkdown turns assistant/user content into safe HTML using the
// vendored marked (GFM: tables, task lists, nested lists, strikethrough,
// autolinks). marked deliberately ships without HTML sanitization, so two
// of our own layers keep things safe:
//
//   1. We HTML-escape the input before parsing, then re-allow `>` so
//      blockquotes still work. A stray `>` can't form an opening tag
//      because `<` stays escaped, so any raw `<script>` in user content
//      lands as literal text.
//
//   2. We strip risky URL schemes (javascript:/data:/vbscript:) from any
//      <a>/<img> in marked's output before handing it to the DOM.
//
// Newlines follow CommonMark: single \n = space, blank line = paragraph
// break. Users who want a visible break hit Enter twice.
window.marked.setOptions({ gfm: true, breaks: false, silent: true });

function renderMarkdown(md) {
  if (renderMode === 'raw') {
    return '<pre class="raw-markdown">' + esc(md || '') + '</pre>';
  }
  let s = String(md || '');
  s = s.replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
  s = s.replace(/&gt;/g, '>');
  let html = window.marked.parse(s);
  html = html.replace(
    /(<(?:a|img)\b[^>]*?\b(?:href|src)=")\s*(?:javascript|data|vbscript)\s*:[^"]*"/gi,
    '$1#unsafe"',
  );
  html = html.replace(/<a /g, '<a target="_blank" rel="noopener" ');
  return html;
}

// Re-render every element that carries a data-raw markdown source. Called
// whenever the user toggles md/raw mode. Streaming response stays connected
// because we don't tear down the SSE — only the in-place HTML changes.
function rerenderAllContent() {
  document.querySelectorAll('[data-raw]').forEach(el => {
    el.innerHTML = renderMarkdown(el.dataset.raw);
  });
}

function setRenderMode(mode) {
  renderMode = mode === 'raw' ? 'raw' : 'md';
  localStorage.setItem('usher.renderMode', renderMode);
  updateRenderModeBtn();
  rerenderAllContent();
}

function updateRenderModeBtn() {
  if (!renderModeBtn) return;
  const val = document.getElementById('render-mode-val');
  if (val) val.textContent = renderMode;
  renderModeBtn.setAttribute('aria-pressed', renderMode === 'raw');
}

if (renderModeBtn) {
  renderModeBtn.addEventListener('click', () => {
    setRenderMode(renderMode === 'md' ? 'raw' : 'md');
  });
  updateRenderModeBtn();
}

function fmt(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return '';
  return d.toLocaleString();
}
function timeNow() {
  return new Date().toLocaleTimeString();
}

function closeES() {
  if (currentES) { currentES.close(); currentES = null; }
  closeScreenES();
}
function closeScreenES() {
  if (currentScreenES) { currentScreenES.close(); currentScreenES = null; }
}
function clearListInterval() {
  if (listInterval) { clearInterval(listInterval); listInterval = null; }
}

window.addEventListener('hashchange', route);

function route() {
  const hash = location.hash || '#/';
  if (hash === '#/' || hash === '') {
    showList();
  } else if (hash === '#/new') {
    showNewSession();
  } else if (hash === '#/chat' || hash.startsWith('#/chat/')) {
    const id = hash === '#/chat' ? 'default' : decodeURIComponent(hash.slice('#/chat/'.length));
    showMainChat(id);
  } else if (hash.startsWith('#/s/')) {
    showDetail(decodeURIComponent(hash.slice(4)));
  }
  updateSidebarActive();
}

// ---------- Sidebar ----------
//
// Polled every 5s independently of the active route. Renders Claude Code
// sessions grouped by cwd, recent activity first. The "main chat" entry is
// static markup in index.html — no fetch needed since we only ever route
// to a single mainchat (id=default).

async function loadSidebar() {
  try {
    // Always fetch include_archived=1 so the count and per-cwd disclosure
    // can show how many are archived even when collapsed. Payload size is
    // trivial at this scale.
    const res = await fetch('/api/sessions?include_archived=1');
    const sessions = res.ok ? (await res.json() || []) : [];
    renderSidebarSessions(sessions);
    updateSidebarActive();
  } catch {/* server may be down briefly */}
}

function renderSidebarSessions(allSessions) {
  const wrap = document.getElementById('sidebar-sessions');
  const count = document.getElementById('sidebar-session-count');
  const visible = allSessions.filter(s => !s.archived);
  if (count) {
    count.textContent = visible.length === allSessions.length
      ? '(' + allSessions.length + ')'
      : '(' + visible.length + '/' + allSessions.length + ')';
  }
  if (!wrap) return;
  if (!allSessions.length) {
    wrap.innerHTML = '<div class="sidebar-empty">no sessions found</div>';
    lastSidebarHtml = '';
    return;
  }
  // Group ALL sessions by cwd (incl. archived) so each group's tail can
  // offer a "└[ N archived ]" disclosure without a separate count lookup.
  const groups = new Map();
  for (const s of allSessions) {
    const cwd = s.cwd || '(unknown)';
    if (!groups.has(cwd)) groups.set(cwd, []);
    groups.get(cwd).push(s);
  }
  // Per design decision (a): cwds whose every session is archived simply
  // disappear from the sidebar. They can still be reached by URL; if a
  // user needs to recover one we may add a bottom "+ N from archived
  // cwds" affordance later, but not yet.
  const cwds = [...groups.keys()]
    .filter(cwd => groups.get(cwd).some(s => !s.archived));
  if (!cwds.length) {
    wrap.innerHTML = '<div class="sidebar-empty">no sessions found</div>';
    lastSidebarHtml = '';
    return;
  }
  const recencyOf = arr => Math.max(...arr.map(s => Date.parse(s.last_event_at) || 0));
  const byRecent = (a, b) => (Date.parse(b.last_event_at) || 0) - (Date.parse(a.last_event_at) || 0);
  // Sort cwd groups by their most-recent visible activity, not absolute
  // — stale cwds with one expanded archived row shouldn't jump to the top.
  cwds.sort((a, b) => {
    const av = groups.get(a).filter(s => !s.archived);
    const bv = groups.get(b).filter(s => !s.archived);
    return recencyOf(bv) - recencyOf(av);
  });

  const renderItem = s => {
    const href = '#/s/' + encodeURIComponent(s.id);
    const dot = statusDot(s.status);
    const auto = s.auto_approve
      ? '<span class="auto-dot" title="auto-approve enabled">⚡</span>'
      : '';
    const title = s.title || '(untitled)';
    const liClass = s.archived ? 'sidebar-item archived-row' : 'sidebar-item';
    return `<li class="${liClass}">
      <a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${dot}${auto}${esc(title)}</a>
      <button class="kebab-btn" type="button"
        data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}"
        aria-label="session actions" title="more">⋮</button>
    </li>`;
  };

  const html = cwds.map(cwd => {
    const all = groups.get(cwd);
    const visibleItems = all.filter(s => !s.archived).sort(byRecent);
    const archivedItems = all.filter(s => s.archived).sort(byRecent);
    const expanded = cwdExpanded.has(cwd);
    let lis = visibleItems.map(renderItem).join('');
    if (expanded) lis += archivedItems.map(renderItem).join('');
    const toggleRow = archivedItems.length === 0
      ? ''
      : `<button class="cwd-toggle-archived" type="button" data-cwd="${esc(cwd)}">${
          expanded ? '└ [ collapse ]' : '└ [ ' + archivedItems.length + ' archived ]'
        }</button>`;
    return `<div class="cwd-group">
      <div class="cwd-label" title="${esc(cwd)}">${esc(cwd)}</div>
      <ul class="sidebar-list">${lis}</ul>
      ${toggleRow}
    </div>`;
  }).join('');
  // Only touch the DOM when the rendered markup actually changed, so a steady
  // session's status-dot animation keeps running smoothly across polls.
  if (html === lastSidebarHtml) return;
  lastSidebarHtml = html;
  wrap.innerHTML = html;
}

function updateSidebarActive() {
  const hash = location.hash || '#/';
  const inMainChat = hash === '#/chat' || hash.startsWith('#/chat/');
  document.querySelectorAll('.sidebar-mainchat').forEach(a => {
    a.classList.toggle('active', inMainChat);
  });
  document.querySelectorAll('.sidebar-new').forEach(a => {
    a.classList.toggle('active', hash === '#/new');
  });
  let sessionKey = '';
  if (hash.startsWith('#/s/')) {
    sessionKey = 's:' + decodeURIComponent(hash.slice(4));
  }
  document.querySelectorAll('#sidebar a[data-route]').forEach(a => {
    a.classList.toggle('active', a.dataset.route === sessionKey);
  });
}

// Mobile sidebar toggle. The sidebar is fixed-position with a slide-in
// transform under 720px wide; the hamburger button toggles .open.
const mobileToggle = document.getElementById('mobile-toggle');
const sidebarEl = document.getElementById('sidebar');
if (mobileToggle && sidebarEl) {
  mobileToggle.addEventListener('click', () => sidebarEl.classList.toggle('open'));
  window.addEventListener('hashchange', () => sidebarEl.classList.remove('open'));
}
const sidebarBackdrop = document.getElementById('sidebar-backdrop');
if (sidebarBackdrop && sidebarEl) {
  sidebarBackdrop.addEventListener('click', () => sidebarEl.classList.remove('open'));
}

// Kebab popover. A single floating element lives at document level and
// is repositioned on each open; closing on outside click, Esc, scroll,
// or resize keeps it tied to the source button without an observer.
const kebabPopover = document.getElementById('kebab-popover');
let kebabOpenFor = null; // session id currently anchored

let kebabOpenBtn = null; // the button element currently anchoring the popover

function closeKebabPopover() {
  if (!kebabPopover) return;
  kebabPopover.hidden = true;
  kebabPopover.innerHTML = '';
  if (kebabOpenBtn) kebabOpenBtn.classList.remove('open');
  kebabOpenBtn = null;
  kebabOpenFor = null;
}

function openKebabPopover(btn) {
  if (!kebabPopover) return;
  const id = btn.dataset.id;
  const archived = btn.dataset.archived === '1';
  const action = archived ? 'unarchive' : 'archive';
  kebabPopover.innerHTML =
    `<button type="button" class="kebab-item" data-action="${action}" data-id="${esc(id)}">${action}</button>`;
  kebabPopover.hidden = false;
  // Position below-right of the button; clamp to viewport edges so the
  // menu stays fully visible on narrow screens.
  const r = btn.getBoundingClientRect();
  const popW = kebabPopover.offsetWidth;
  const popH = kebabPopover.offsetHeight;
  let left = r.right - popW;
  let top = r.bottom + 4;
  if (left < 4) left = 4;
  if (top + popH > window.innerHeight - 4) {
    top = r.top - popH - 4;
  }
  kebabPopover.style.left = left + 'px';
  kebabPopover.style.top = top + 'px';
  kebabOpenFor = id;
  // .open keeps the kebab visible after the cursor leaves its row — the
  // user is now interacting with the popover, not the row.
  btn.classList.add('open');
  kebabOpenBtn = btn;
}

document.addEventListener('click', (e) => {
  const cwdToggle = e.target.closest('.cwd-toggle-archived');
  if (cwdToggle) {
    e.preventDefault();
    const cwd = cwdToggle.dataset.cwd;
    if (cwdExpanded.has(cwd)) cwdExpanded.delete(cwd);
    else cwdExpanded.add(cwd);
    loadSidebar();
    return;
  }
  const kebab = e.target.closest('.kebab-btn');
  if (kebab) {
    e.preventDefault();
    e.stopPropagation();
    if (kebabOpenFor === kebab.dataset.id) {
      closeKebabPopover();
    } else {
      openKebabPopover(kebab);
    }
    return;
  }
  const item = e.target.closest('.kebab-item');
  if (item) {
    e.preventDefault();
    e.stopPropagation();
    handleKebabAction(item.dataset.action, item.dataset.id);
    return;
  }
  if (kebabOpenFor && !e.target.closest('#kebab-popover')) {
    closeKebabPopover();
  }
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && kebabOpenFor) closeKebabPopover();
});
window.addEventListener('resize', closeKebabPopover);
// Listen on the sidebar's scroll container so a long sidebar scroll
// doesn't leave the popover floating mid-air.
if (sidebarEl) sidebarEl.addEventListener('scroll', closeKebabPopover, { passive: true });

async function handleKebabAction(action, id) {
  closeKebabPopover();
  const method = action === 'archive' ? 'POST' : 'DELETE';
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/archive', { method });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('archive/unarchive failed', e);
  }
}

// ---------- New session view ----------
//
// Mirrors the regular session detail layout so the page transition after
// creation is purely additive (empty placeholders fill in). The only
// pre-creation difference is the auto-approve toggle position — replaced
// with a cwd picker, since auto-approve can't be set without a session id.
// Submitting POSTs to /api/sessions (router.StartSession returns the new
// id immediately and streams to broker subscribers), then hash-routes to
// the freshly-created session's detail page.

async function showNewSession() {
  clearListInterval();
  currentDetailId = null;
  closeES();
  subtitle.textContent = 'new session';

  let cwds = [];
  try {
    const res = await fetch('/api/sessions');
    if (res.ok) {
      const data = (await res.json()) || [];
      cwds = [...new Set(data.map(s => s.cwd).filter(Boolean))].sort();
    }
  } catch {/* offline → datalist just empty */}

  const options = cwds.map(c => `<option value="${esc(c)}"></option>`).join('');
  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <section class="send-anchor">
        <div class="input-row">
          <textarea id="prompt" placeholder="message…"></textarea>
          <button id="send">send</button>
        </div>
        <div class="send-controls">
          <label class="new-cwd-field">
            <span class="muted">cwd</span>
            <input id="new-cwd" type="text" list="new-cwd-list" autocomplete="off"
                   placeholder="/absolute/path/to/project">
            <datalist id="new-cwd-list">${options}</datalist>
          </label>
        </div>
        <div id="new-session-err" class="err" style="display:none; margin-top:0.5rem"></div>
      </section>
    </div>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const cwdEl = document.getElementById('new-cwd');
  const errEl = document.getElementById('new-session-err');
  cwdEl.focus();

  const submit = async () => {
    if (sendBtn.disabled) return; // re-entrancy guard during in-flight submit
    errEl.style.display = 'none';
    errEl.textContent = '';
    sendBtn.disabled = true;
    sendBtn.textContent = 'creating…';
    try {
      const res = await fetch('/api/sessions', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          cwd: cwdEl.value.trim(),
          initial_message: promptEl.value,
        }),
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
      // Refresh sidebar so the new session shows up while its first stream
      // is still in flight; fsnotify normally surfaces it within ~1s but a
      // proactive reload avoids the awkward "where did it go?" gap.
      loadSidebar();
      location.hash = '#/s/' + encodeURIComponent(body.id);
    } catch (ex) {
      errEl.textContent = String(ex.message || ex);
      errEl.style.display = '';
      sendBtn.disabled = false;
      sendBtn.textContent = 'send';
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
}

// ---------- List view ----------

async function showList() {
  closeES();
  currentDetailId = null;
  subtitle.textContent = 'discovered Claude Code sessions';
  // Stable shell: pinned controls + a .list-scroll wrapper (the scroll
  // container — <main> is overflow:hidden). loadList only swaps the rows, so
  // the 5s poll doesn't disturb the controls.
  root.innerHTML = `
    <div class="list-controls">
      <select id="list-cwd"><option value="">all folders</option></select>
      <label class="archived-toggle"><input type="checkbox" id="list-archived"> show archived</label>
    </div>
    <div class="list-scroll">
      <table>
        <colgroup><col><col class="col-cwd"><col class="col-when"><col class="col-act"></colgroup>
        <thead><tr><th>title</th><th>cwd</th><th>last activity</th><th aria-label="actions"></th></tr></thead>
        <tbody id="list-rows"></tbody>
      </table>
    </div>`;
  lastListRowsHtml = '';
  lastCwdSig = '';
  const cEl = document.getElementById('list-cwd');
  if (cEl) cEl.addEventListener('change', applyListFilter);
  const aEl = document.getElementById('list-archived');
  if (aEl) aEl.addEventListener('change', applyListFilter);
  if (!listInterval) listInterval = setInterval(loadList, 5000);
  await loadList();
}

// applyListFilter hides rows that don't match the cwd dropdown and the
// archived toggle. All client-side — the rows always include archived sessions.
function applyListFilter() {
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return;
  const cEl = document.getElementById('list-cwd');
  const aEl = document.getElementById('list-archived');
  const cwd = cEl ? cEl.value : '';
  const showArchived = aEl ? aEl.checked : false;
  rowsEl.querySelectorAll('tr[data-id]').forEach(tr => {
    const okCwd = !cwd || tr.dataset.cwd === cwd;
    const okArchived = showArchived || tr.dataset.archived !== '1';
    tr.style.display = (okCwd && okArchived) ? '' : 'none';
  });
}

// updateCwdOptions rebuilds the cwd <select> from the distinct cwds, but only
// when the set changed, so the 5s poll doesn't reset the user's selection.
function updateCwdOptions(cwds) {
  const sel = document.getElementById('list-cwd');
  if (!sel) return;
  const sig = cwds.join('\n');
  if (sig === lastCwdSig) return;
  lastCwdSig = sig;
  const cur = sel.value;
  sel.innerHTML = '<option value="">all folders</option>' +
    cwds.map(c => `<option value="${esc(c)}">${esc(c)}</option>`).join('');
  if (cwds.includes(cur)) sel.value = cur;
}

async function loadList() {
  if (location.hash && location.hash !== '#/' && location.hash !== '') return;
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return; // shell not built (not on the list view)
  try {
    // Always fetch the full set (incl. archived); the controls narrow it
    // client-side, so toggling "show archived" needs no refetch.
    const res = await fetch('/api/sessions?include_archived=1');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = (await res.json()) || [];
    const html = data.length ? data.map(s => {
      const title = s.title || '(untitled)';
      const dot = statusDot(s.status);
      return `
      <tr data-id="${esc(s.id)}" data-cwd="${esc(s.cwd || '')}" data-archived="${s.archived ? '1' : ''}" class="${s.archived ? 'archived' : ''}">
        <td class="title" title="${esc(title)}">${dot ? dot + ' ' : ''}${esc(title)}</td>
        <td class="cwd" title="${esc(s.cwd || '')}">${esc(s.cwd || '')}</td>
        <td>${esc(fmt(s.last_event_at))}</td>
        <td class="act"><button class="kebab-btn" type="button" data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}" aria-label="session actions" title="more">⋮</button></td>
      </tr>`;
    }).join('') : '<tr><td colspan="4" class="muted" style="padding:0.75rem">no sessions found</td></tr>';
    // Skip the rebuild when unchanged so status-dot animations don't restart
    // and the current filter view is left untouched.
    if (html === lastListRowsHtml) return;
    lastListRowsHtml = html;
    rowsEl.innerHTML = html;
    rowsEl.querySelectorAll('tr[data-id]').forEach(tr => {
      tr.addEventListener('click', (e) => {
        // Kebab clicks are taken by its own (document-level) popover handler;
        // the row listener runs first while bubbling, so skip them here.
        if (e.target.closest('.kebab-btn')) return;
        location.hash = '#/s/' + encodeURIComponent(tr.dataset.id);
      });
    });
    updateCwdOptions([...new Set(data.map(s => s.cwd).filter(Boolean))].sort());
    applyListFilter(); // keep the active filters applied across polls
  } catch (e) {
    rowsEl.innerHTML = '<tr><td colspan="4" class="err" style="padding:0.75rem">failed to load: ' + esc(String(e)) + '</td></tr>';
    lastListRowsHtml = '';
  }
}

// ---------- Detail view ----------

async function showDetail(id) {
  clearListInterval();
  closeES();
  // Fresh view: reset sync state so a prior session's signature/stream flag
  // can't suppress this one's first render.
  currentDetailId = id;
  lastTranscriptSig = '';
  renderedTurns = [];
  transcriptLimit = TRANSCRIPT_PAGE;
  transcriptTotal = 0;
  detailStreaming = false;
  subtitle.textContent = 'session detail';

  let sess;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id));
    if (!res.ok) {
      root.innerHTML = '<div class="err">session not found</div>';
      return;
    }
    sess = await res.json();
  } catch (e) {
    root.innerHTML = '<div class="err">' + esc(String(e)) + '</div>';
    return;
  }

  // Show title / cwd / short id in the page header subtitle so it stays
  // visible while transcript / response sections scroll. Mirrors how main
  // chat surfaces its focus block — the page header is the only sticky
  // band, no fragile second-tier sticky element.
  renderSessionSubtitle(sess);

  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <div class="chat-loading muted" style="padding:0.5rem">loading…</div>
      <section class="send-anchor">
        <div id="term-panel" class="term-panel" hidden>
          <div class="term-screen"><pre id="term-grid" class="term-grid muted">connecting…</pre></div>
          <div class="term-keys">
            <button type="button" data-key="escape">esc</button>
            <button type="button" data-key="tab">tab</button>
            <button type="button" data-key="up" aria-label="up">↑</button>
            <button type="button" data-key="down" aria-label="down">↓</button>
            <button type="button" data-key="left" aria-label="left">←</button>
            <button type="button" data-key="right" aria-label="right">→</button>
            <button type="button" data-key="enter">⏎</button>
          </div>
        </div>
        <div class="input-row">
          <textarea id="prompt" placeholder="message…"></textarea>
          <button id="send">send</button>
          <button id="cancel" class="cancel" hidden>cancel</button>
        </div>
        <div class="send-controls">
          <button id="auto-approve-toggle" class="auto-approve-toggle" type="button"
            aria-pressed="${sess.auto_approve ? 'true' : 'false'}"
            title="when on, every PreToolUse hook for this session is allowed without prompting">
            auto-approve: ${sess.auto_approve ? 'on' : 'off'}
          </button>
          <button id="term-toggle" class="term-toggle" type="button" aria-pressed="false"
            title="terminal mirror — click to cycle off → auto → on. auto reveals on send and hides when you scroll up into history; on stays pinned open.">
            terminal: off
          </button>
        </div>
      </section>
    </div>
  `;

  await loadTranscript(id);

  const chatEl = document.getElementById('chat-scroll');
  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const cancelBtn = document.getElementById('cancel');

  const autoBtn = document.getElementById('auto-approve-toggle');
  if (autoBtn) {
    autoBtn.addEventListener('click', async () => {
      const next = autoBtn.getAttribute('aria-pressed') !== 'true';
      autoBtn.disabled = true;
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/auto-approve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: next }),
        });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        autoBtn.setAttribute('aria-pressed', next ? 'true' : 'false');
        autoBtn.textContent = 'auto-approve: ' + (next ? 'on' : 'off');
        loadSidebar(); // refresh sidebar marker immediately
      } catch (e) {
        appendChatMessage({ role: 'error', content: 'auto-approve toggle failed: ' + String(e), ts: new Date().toISOString() });
      } finally {
        autoBtn.disabled = false;
      }
    });
  }

  // Terminal mirror: a collapsible live pane docked above the input, driven by
  // a 3-state toggle that cycles off → auto → on (persisted in localStorage):
  //   off  — hidden, no automatic behaviour (default)
  //   auto — reveals on send (the un-flushed turn is starting) and hides again
  //          after a deliberate scroll up into history; re-reveals next send
  //   on   — pinned open, never auto-hidden
  // The /screen stream runs only while actually shown, so background sessions
  // don't poll capture-pane. Soft keys wire once (the grid node is permanent).
  const termToggle = document.getElementById('term-toggle');
  const termPanel = document.getElementById('term-panel');
  let termMode = (() => { try { return localStorage.getItem('usher.term.mode') || 'off'; } catch { return 'off'; } })();
  let termAutoShown = false; // auto-mode runtime reveal (set by send, cleared by scroll)
  let termShown = false;     // whether the panel is currently streaming/visible
  let termAppliedRows = 0;   // rows last applied — gates needless restreams
  let softKeysWired = false;
  // Pane/box rows by mode (the client owns this policy; the server just clamps).
  // auto is a fixed compact glance; on fills half the chat area, bounded so the
  // input/controls below stay visible. The pane AND the viewer box are both
  // sized from this one number, so they can't drift.
  const TERM_ROW_PX = 16.25; // .term-grid 13px × line-height 1.25 (keep in sync with CSS)
  const targetRows = (mode) => {
    if (mode !== 'on') return 10;
    const chatH = chatEl.clientHeight;
    const roomRows = Math.floor((chatH - 6 * 16) / TERM_ROW_PX); // leave ~6rem for input
    const halfRows = Math.floor((chatH / 2) / TERM_ROW_PX);
    return Math.max(1, Math.min(halfRows, roomRows));
  };
  // applyTermVisibility resolves termMode + termAutoShown into the shown/hidden
  // state and the pane/box size. The /screen restream is what re-applies a new
  // pane size, so it's skipped when nothing changed (e.g. a send re-asserting an
  // already-open auto panel) — only a real reveal or size change restreams.
  const applyTermVisibility = () => {
    const show = termMode === 'on' || (termMode === 'auto' && termAutoShown);
    if (!show) {
      if (termShown) { termShown = false; if (termPanel) termPanel.hidden = true; closeScreenES(); }
      return;
    }
    const rows = targetRows(termMode);
    if (termShown && rows === termAppliedRows) return; // already shown at this size
    termShown = true;
    termAppliedRows = rows;
    if (termPanel) termPanel.hidden = false;
    if (!softKeysWired) { wireSoftKeys(id); softKeysWired = true; }
    const gridEl = document.getElementById('term-grid');
    if (gridEl) {
      const box = gridEl.parentElement;
      // content-box: max-height is the content area, so rows×ROW_PX shows exactly
      // `rows` lines (+a few px slack to avoid a sub-pixel scrollbar).
      box.style.maxHeight = Math.ceil(rows * TERM_ROW_PX) + 4 + 'px';
      openScreenStream(id, gridEl, measureCols(box), rows);
    }
    // The panel grows the sticky anchor; keep the chat pinned if it was.
    if (isNearBottom(chatEl)) chatEl.scrollTop = chatEl.scrollHeight;
  };
  // applyTermMode reflects termMode onto the button, then resolves visibility.
  const applyTermMode = () => {
    if (!termToggle) return;
    termToggle.setAttribute('data-mode', termMode);
    termToggle.setAttribute('aria-pressed', termMode === 'off' ? 'false' : 'true');
    termToggle.textContent = 'terminal: ' + termMode;
    applyTermVisibility();
  };
  if (termToggle && termPanel) {
    termToggle.addEventListener('click', () => {
      termMode = termMode === 'off' ? 'auto' : termMode === 'auto' ? 'on' : 'off';
      try { localStorage.setItem('usher.term.mode', termMode); } catch { /* private mode */ }
      // Entering auto reveals immediately (click feedback); the auto rules take
      // over from there. Leaving auto clears the runtime reveal flag.
      termAutoShown = termMode === 'auto';
      applyTermMode();
    });
    applyTermMode(); // honour the persisted mode on open ('on' reveals now)
  }

  // Auto-hide on scroll (auto mode only): a deliberate scroll up into history
  // dismisses the mirror. Listen to wheel/touch *intent*, not the 'scroll' event
  // — the stick-to-bottom auto-scrolls fire 'scroll' too and would self-hide
  // mid-stream. Accumulate upward travel; a downward gesture breaks the run.
  let termUpAccum = 0;
  const TERM_HIDE_SCROLL = 300; // px of cumulative upward gesture to auto-hide
  const onTermScroll = (deltaY) => {
    if (termMode !== 'auto' || !termShown) { termUpAccum = 0; return; }
    if (deltaY < 0) {
      termUpAccum += -deltaY;
      if (termUpAccum >= TERM_HIDE_SCROLL) {
        termUpAccum = 0;
        termAutoShown = false;
        applyTermMode();
      }
    } else {
      termUpAccum = 0;
    }
  };
  // Only count gestures over the chat itself — scrolling inside the terminal
  // pane (which lives within chat-scroll) must not dismiss it.
  const inTerm = (e) => termPanel && termPanel.contains(e.target);
  chatEl.addEventListener('wheel', (e) => { if (!inTerm(e)) onTermScroll(e.deltaY); }, { passive: true });
  let termTouchY = null;
  chatEl.addEventListener('touchstart', (e) => { termTouchY = (!inTerm(e) && e.touches[0]) ? e.touches[0].clientY : null; }, { passive: true });
  chatEl.addEventListener('touchmove', (e) => {
    if (termTouchY == null || !e.touches[0] || inTerm(e)) return;
    const y = e.touches[0].clientY;
    onTermScroll(termTouchY - y); // finger down (y increases) = content scrolls up = negative
    termTouchY = y;
  }, { passive: true });

  cancelBtn.addEventListener('click', async () => {
    cancelBtn.disabled = true;
    try {
      await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', { method: 'DELETE' });
    } catch (e) {
      appendChatMessage({ role: 'error', content: 'cancel failed: ' + String(e), ts: new Date().toISOString() });
    } finally {
      cancelBtn.disabled = false;
    }
  });

  // Shared state between submit and the SSE handlers: lets the exit event
  // canonicalize the optimistic user node's timestamp from server-side ts.
  const turnState = { userNode: null };
  openEventStream(id, chatEl, sendBtn, cancelBtn, turnState);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    // Auto mode: the turn is starting, so its un-flushed output is about to
    // appear — reveal the mirror now (no-op in off/on).
    if (termMode === 'auto') { termAutoShown = true; applyTermMode(); }
    // Optimistic: show user message immediately. Assistant placeholder is
    // created by openEventStream on subprocess.started. Marked .optimistic so
    // the turn-end reconcile drops it in favor of the canonical user turn.
    turnState.userNode = appendChatMessage({ role: 'user', content: text });
    if (turnState.userNode) turnState.userNode.classList.add('optimistic');
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        appendChatMessage({ role: 'error', content: 'send failed: ' + (err.error || ('HTTP ' + res.status)), ts: new Date().toISOString() });
        sendBtn.disabled = false;
        return;
      }
      // button stays disabled until subprocess.exit handler re-enables
    } catch (e) {
      appendChatMessage({ role: 'error', content: String(e), ts: new Date().toISOString() });
      sendBtn.disabled = false;
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  promptEl.focus();
}

// statusDot renders the sidebar run-state indicator: a dim green dot when
// usher holds a warm-but-idle process ("live"), and a brighter pulsing dot
// while a turn is executing ("running"). Idle/undiscovered sessions get none.
function statusDot(status) {
  if (status === 'running') return '<span class="running-dot executing" title="executing">●</span>';
  if (status === 'live') return '<span class="running-dot" title="process live">●</span>';
  return '';
}

// openEventStream attaches SSE handlers to /api/sessions/{id}/events. The
// in-flight assistant turn renders inline at the bottom of the transcript:
// subprocess.started appends a placeholder chat-message; each 'assistant'
// event (message granularity — the session jsonl is tailed, not a stream-json
// token feed) accumulates its text into the placeholder; subprocess.exit
// finalizes it. Turn errors surface via the 'error' event. Other jsonl lines
// ('user', 'system', bookkeeping) are dropped as diagnostic noise.
function openEventStream(id, chatEl, sendBtn, cancelBtn, turnState) {
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/events');
  currentES = es;

  let placeholder = null;
  let accum = '';
  let opened = false;
  // On reconnect (not the first connect) re-fetch to fill any gap — but never
  // mid-turn, where the live stream owns the bubble.
  es.onopen = () => {
    if (opened && !detailStreaming) loadTranscript(id);
    opened = true;
  };

  const setLoadEarlierDisabled = (v) => {
    const b = document.querySelector('#chat-scroll > .load-earlier');
    if (b) b.disabled = v;
  };
  const onIdle = () => {
    detailStreaming = false;
    sendBtn.disabled = false;
    if (cancelBtn) cancelBtn.hidden = true;
    setLoadEarlierDisabled(false);
  };
  const onRunning = () => {
    detailStreaming = true;
    sendBtn.disabled = true;
    if (cancelBtn) cancelBtn.hidden = false;
    setLoadEarlierDisabled(true);
  };

  const setRoleText = (el, text) => {
    const roleEl = el && el.querySelector('.role');
    if (roleEl && roleEl.firstChild) roleEl.firstChild.textContent = text;
  };
  const setContent = (el, raw) => {
    const contentEl = el && el.querySelector('.content');
    if (!contentEl) return;
    contentEl.dataset.raw = raw;
    contentEl.innerHTML = renderMarkdown(raw);
  };
  // assistantText joins the text blocks of an 'assistant' jsonl event's
  // message, skipping thinking / tool_use blocks.
  const assistantText = (d) => {
    const content = d && d.message && d.message.content;
    if (!Array.isArray(content)) return '';
    return content.filter((b) => b && b.type === 'text').map((b) => b.text || '').join('');
  };

  const handlers = {
    'subprocess.started': () => {
      accum = '';
      placeholder = appendChatMessage({ role: 'assistant', content: '' });
      if (placeholder) placeholder.classList.add('optimistic');
      onRunning();
    },
    'assistant': (d) => {
      // Message granularity: each assistant turn carries its full text blocks
      // (no token deltas). A turn may produce several assistant messages (text
      // before a tool call, then more after); accumulate their text into the
      // one placeholder. Tool-only messages have no text and are skipped.
      const text = assistantText(d);
      if (!text) return;
      // subprocess.started normally creates the bubble, but a session's first
      // turn begins before this SSE subscribes (StartSession publishes at once
      // and the broker has no replay), so that event is missed. Create the
      // bubble lazily on the first assistant text so turn 1 streams like the rest.
      if (!placeholder) {
        placeholder = appendChatMessage({ role: 'assistant', content: '' });
        if (placeholder) placeholder.classList.add('optimistic');
        onRunning();
      }
      // Follow the streaming text only if the reader is at the bottom; check
      // before setContent grows the bubble.
      const stick = isNearBottom(chatEl);
      accum += (accum ? '\n' : '') + text;
      setContent(placeholder, accum);
      if (stick) chatEl.scrollTop = chatEl.scrollHeight;
    },
    'subprocess.exit': (d) => {
      // Canonicalize timestamps from server-persisted jsonl (set by
      // router.enrichExitWithTurnTimestamps). Replaces the optimistic
      // client-side "now" stamps we showed during the turn. The exit payload
      // carries {stop_reason, user_ts, assistant_ts} — turn errors arrive via
      // the separate 'error' event, so exit always means a clean finish here.
      if (turnState && turnState.userNode) {
        updateMessageTs(turnState.userNode, d.user_ts);
        turnState.userNode = null;
      }
      if (placeholder) {
        updateMessageTs(placeholder, d.assistant_ts || new Date().toISOString());
        placeholder = null;
      }
      onIdle();
      // Reconcile to the canonical transcript: the live stream shows only
      // assistant text, so this pulls in tool calls/results and any turn the
      // SSE didn't witness from the start.
      loadTranscript(id);
      // A just-created session opened as "(untitled)"; by now the server has
      // its real title/cwd, so refresh the header too.
      refreshSubtitle(id);
    },
    'error': (d) => {
      const msg = d.message || JSON.stringify(d);
      if (placeholder) {
        updateMessageTs(placeholder, new Date().toISOString());
        // Keep .optimistic: an error isn't a canonical transcript turn, so the
        // next reconcile should clear it (matching the old full-rebuild).
        placeholder.className = 'chat-message error optimistic';
        setRoleText(placeholder, 'error');
        setContent(placeholder, msg);
        placeholder = null;
      } else {
        const n = appendChatMessage({ role: 'error', content: msg, ts: new Date().toISOString() });
        if (n) n.classList.add('optimistic');
      }
      onIdle();
    },
    // Dropped (diagnostic noise): 'user' (our own prompt / tool results),
    // 'system' init/status, and other jsonl bookkeeping lines.
  };

  Object.entries(handlers).forEach(([name, fn]) => {
    es.addEventListener(name, (ev) => {
      // addEventListener('error', …) also catches EventSource's native
      // connection-error event (a plain Event on every disconnect/reconnect),
      // which has no .data. Server-sent events always carry data:, so a
      // missing payload means a native error — ignore it (es.onerror handles
      // reconnect) instead of rendering a spurious "{}" error bubble.
      if (ev.data == null) return;
      let data;
      try { data = JSON.parse(ev.data); } catch { data = { raw: ev.data }; }
      try { fn(data); } catch (e) { console.error('handler error', name, e); }
    });
  });

  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
}

// ---------- Terminal mirror (read-only) ----------
//
// A1 escape hatch: an inline, collapsible panel above the chat input that
// mirrors the session's live tmux pane (claude's TUI) as a periodically
// re-captured frame over SSE, plus a row of soft keys so the user can drive
// menus the curated send path can't reach (arrow navigation, esc, ctrl-c).
// Read-only otherwise — no arbitrary typing; that's what chat is for.

// openScreenStream subscribes to the pane mirror SSE. Each `screen` event
// carries the full re-captured frame (server only sends it when it changed);
// `nopane` means usher holds no live window. EventSource auto-reconnects.
// Tracked in currentScreenES (not currentES) so it coexists with the detail
// view's /events stream.
function openScreenStream(id, screenEl, cols, rows) {
  closeScreenES();
  const params = [];
  if (cols) params.push('cols=' + cols);
  if (rows) params.push('rows=' + rows);
  const q = params.length ? ('?' + params.join('&')) : '';
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/screen' + q);
  currentScreenES = es;
  let lastRaw = null;
  es.addEventListener('screen', (ev) => {
    if (ev.data == null) return;
    let s;
    try { s = JSON.parse(ev.data); } catch { return; }
    if (s === lastRaw) return;
    lastRaw = s;
    const wrap = screenEl.parentElement;
    // Keep the view pinned to the bottom only if the reader is already there,
    // so scrolling up to inspect output isn't yanked back on the next frame.
    const atBottom = wrap ? (wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 40) : true;
    screenEl.classList.remove('muted');
    screenEl.innerHTML = ansiToHtml(trimTrailingBlankLines(s));
    if (atBottom && wrap) wrap.scrollTop = wrap.scrollHeight;
  });
  es.addEventListener('nopane', () => {
    lastRaw = null;
    screenEl.classList.add('muted');
    screenEl.textContent =
      'no live process for this session — open chat and send a message to start one.';
  });
  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
}

// wireSoftKeys POSTs the tapped key to /keys. The server allow-lists key names;
// the screen stream reflects the result, so we don't render an ack — only a
// brief red flash if the key was rejected (e.g. the pane went away).
function wireSoftKeys(id) {
  document.querySelectorAll('.term-keys button[data-key]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      try {
        const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/keys', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ key: btn.dataset.key }),
        });
        if (!res.ok) {
          btn.classList.add('term-key-err');
          setTimeout(() => btn.classList.remove('term-key-err'), 500);
        }
      } catch {/* transient; the next tap or frame recovers */}
    });
  });
}

// measureCols turns the screen box's available width into terminal columns, so
// the tmux pane can be sized to fit the viewer (measured once, on expand). A
// hidden ruler in the grid's font gives an accurate per-char width; the server
// clamps the result to a sane range.
function measureCols(boxEl) {
  const ruler = document.createElement('span');
  ruler.textContent = '0'.repeat(100);
  ruler.style.cssText =
    'position:absolute;visibility:hidden;white-space:pre;' +
    'font:13px ui-monospace,"SF Mono",Menlo,monospace';
  document.body.appendChild(ruler);
  const charPx = ruler.getBoundingClientRect().width / 100;
  ruler.remove();
  if (!charPx || !boxEl) return 80;
  // clientWidth includes the side padding; trim a little so a full-width line
  // doesn't trip horizontal scroll.
  const cols = Math.floor((boxEl.clientWidth - 24) / charPx);
  return cols > 0 ? cols : 80;
}

// trimTrailingBlankLines drops empty trailing rows from a capture-pane frame so
// the mirror isn't padded out to the full pane height. Blankness is tested
// after stripping SGR escapes so a colour-only-but-empty line still counts.
function trimTrailingBlankLines(s) {
  const lines = String(s).split('\n');
  while (lines.length &&
         lines[lines.length - 1].replace(/\x1b\[[0-9;]*m/g, '').trim() === '') {
    lines.pop();
  }
  return lines.join('\n');
}

// Base 16-colour ANSI palette (30-37 normal, 90-97 bright), toned to read on
// the dark terminal background.
const ANSI_COLORS = [
  '#484f58', '#ff7b72', '#3fb950', '#d29922', '#58a6ff', '#bc8cff', '#39c5cf', '#b1bac4',
  '#6e7681', '#ffa198', '#56d364', '#e3b341', '#79c0ff', '#d2a8ff', '#56d4dd', '#f0f6fc',
];
const TERM_BG = '#0d1117';
const TERM_FG = '#c9d1d9';

// ansiToHtml converts a `capture-pane -e` frame (plain text + SGR colour
// escapes) into HTML spans. capture-pane -e emits only SGR (`ESC [ … m`), so
// that's all we parse: bold/dim/italic/underline/inverse, the 16 base colours,
// 256-colour, and truecolour. Inverse swaps fg/bg — that's how a TUI paints its
// selected menu row, the whole reason this mirror exists.
function ansiToHtml(s) {
  const str = String(s);
  let fg = null, bg = null, bold = false, dim = false, ital = false, ul = false, inv = false;
  let out = '', open = false;
  const color256 = (n) => {
    if (n < 16) return ANSI_COLORS[n];
    if (n >= 232) { const v = 8 + (n - 232) * 10; return `rgb(${v},${v},${v})`; }
    n -= 16;
    const r = Math.floor(n / 36), g = Math.floor((n % 36) / 6), b = n % 6;
    const c = (x) => (x === 0 ? 0 : 55 + x * 40);
    return `rgb(${c(r)},${c(g)},${c(b)})`;
  };
  const closeSpan = () => { if (open) { out += '</span>'; open = false; } };
  const openSpan = () => {
    let f = fg, b = bg;
    if (inv) { f = bg === null ? TERM_BG : bg; b = fg === null ? TERM_FG : fg; }
    const st = [];
    if (f !== null) st.push('color:' + f);
    if (b !== null) st.push('background:' + b);
    if (bold) st.push('font-weight:600');
    if (dim) st.push('opacity:0.6');
    if (ital) st.push('font-style:italic');
    if (ul) st.push('text-decoration:underline');
    if (!st.length) return;
    out += '<span style="' + st.join(';') + '">';
    open = true;
  };
  const esc1 = (c) => (c === '&' ? '&amp;' : c === '<' ? '&lt;' : c === '>' ? '&gt;' : c);
  let i = 0;
  while (i < str.length) {
    const ch = str[i];
    if (ch === '\x1b' && str[i + 1] === '[') {
      const m = /^\x1b\[([0-9;]*)m/.exec(str.slice(i));
      if (m) {
        closeSpan();
        const ps = m[1] === '' ? [0] : m[1].split(';').map(Number);
        for (let k = 0; k < ps.length; k++) {
          const p = ps[k];
          if (p === 0) { fg = bg = null; bold = dim = ital = ul = inv = false; }
          else if (p === 1) bold = true;
          else if (p === 2) dim = true;
          else if (p === 3) ital = true;
          else if (p === 4) ul = true;
          else if (p === 7) inv = true;
          else if (p === 22) bold = dim = false;
          else if (p === 23) ital = false;
          else if (p === 24) ul = false;
          else if (p === 27) inv = false;
          else if (p >= 30 && p <= 37) fg = ANSI_COLORS[p - 30];
          else if (p >= 40 && p <= 47) bg = ANSI_COLORS[p - 40];
          else if (p >= 90 && p <= 97) fg = ANSI_COLORS[p - 90 + 8];
          else if (p >= 100 && p <= 107) bg = ANSI_COLORS[p - 100 + 8];
          else if (p === 39) fg = null;
          else if (p === 49) bg = null;
          else if (p === 38 || p === 48) {
            const mode = ps[k + 1];
            if (mode === 5) { const col = color256(ps[k + 2]); if (p === 38) fg = col; else bg = col; k += 2; }
            else if (mode === 2) { const col = `rgb(${ps[k + 2] || 0},${ps[k + 3] || 0},${ps[k + 4] || 0})`; if (p === 38) fg = col; else bg = col; k += 4; }
          }
        }
        openSpan();
        i += m[0].length;
        continue;
      }
    }
    if (ch === '\x1b') { i++; continue; } // drop a stray/unrecognised escape
    out += esc1(ch);
    i++;
  }
  closeSpan();
  return out;
}

// ---------- Main chat view ----------

async function showMainChat(id) {
  clearListInterval();
  currentDetailId = null;
  closeES();
  subtitle.innerHTML = `<span class="subtitle-left"><strong class="session-title">main chat</strong></span>`;

  root.innerHTML = `
    <div id="chat-scroll" class="chat-area">
      <section class="send-anchor">
        <div class="input-row">
          <textarea id="prompt" placeholder="message… (try /help)"></textarea>
          <button id="send">send</button>
        </div>
      </section>
    </div>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');

  await loadMainChatInfo(id);
  await loadChatMessages(id);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    // Optimistic: show the user's message immediately and a "thinking" placeholder
    // since LLM agents may take 5–30s before any response comes back.
    const userNode = appendChatMessage({ role: 'user', content: text });
    const placeholder = appendChatMessage({ role: 'agent', content: 'thinking…', _placeholder: true });
    try {
      const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      const data = await res.json();
      if (placeholder) placeholder.remove();
      if (!res.ok) {
        appendChatMessage({ role: 'error', content: data.error || 'send failed', ts: new Date().toISOString() });
      } else {
        // Server returns the persisted user+agent pair. Canonicalize the
        // optimistic user node's ts from the server, then render the agent
        // reply (which already carries its server ts via appendChatMessage).
        const msgs = data.messages || [];
        const serverUser = msgs.find(m => m.role === 'user');
        if (serverUser && serverUser.ts) updateMessageTs(userNode, serverUser.ts);
        for (const m of msgs.filter(m => m.role !== 'user')) appendChatMessage(m);
        // Focus may have shifted this turn — update the header.
        renderFocus(data.focus);
      }
    } catch (e) {
      if (placeholder) placeholder.remove();
      appendChatMessage({ role: 'error', content: String(e), ts: new Date().toISOString() });
    } finally {
      sendBtn.disabled = false;
      promptEl.focus();
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  promptEl.focus();
}

function renderSessionSubtitle(sess) {
  subtitle.innerHTML =
    `<span class="subtitle-left">` +
      `<strong class="session-title">${esc(sess.title || '(untitled)')}</strong>` +
    `</span>` +
    `<span class="session-id">${esc(sess.id.slice(0, 8))}</span>` +
    `<span class="session-cwd">${esc(sess.cwd || '')}</span>`;
}

// refreshSubtitle re-reads the session so the header picks up a title/cwd that
// filled in after the view opened (a brand-new session opens as "(untitled)";
// the server has the real values once the first turn lands). Guarded by
// currentDetailId so a late fetch can't write into a view already left.
async function refreshSubtitle(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id));
    if (!res.ok) return;
    const sess = await res.json();
    if (id !== currentDetailId) return;
    renderSessionSubtitle(sess);
  } catch {/* ignore */}
}

async function loadTranscript(id, opts) {
  opts = opts || {};
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/transcript?limit=' + transcriptLimit);
    if (!res.ok) return;
    const turns = (await res.json()) || [];
    // A late re-fetch must not render into a view the user already left.
    if (id !== currentDetailId) return;
    const total = parseInt(res.headers.get('X-Transcript-Total') || '', 10);
    transcriptTotal = Number.isFinite(total) ? total : turns.length;
    // Transcripts are append-only, so a change shows up as a longer list or a
    // mutated last turn. Skip the rebuild when nothing changed (no flicker /
    // scroll yank when there's nothing new).
    const last = turns[turns.length - 1];
    const sig = turns.length + ':' + (last ? JSON.stringify(last) : '');
    if (sig === lastTranscriptSig) { updateLoadEarlier(id); return; }
    const el = document.getElementById('chat-scroll');
    // Can't render now (view mid-transition): leave lastTranscriptSig untouched
    // so the next call retries this state instead of skipping it forever.
    if (!el) return;
    // Capture stick-to-bottom intent before any mutation changes the geometry.
    const wasAtBottom = isNearBottom(el);
    // Drop the loading stub and any optimistic bubbles (the in-flight turn's
    // user/assistant placeholders) — they're about to be represented by their
    // canonical turns from this fetch.
    el.querySelectorAll(':scope > .chat-loading, :scope > .chat-message.optimistic').forEach(n => n.remove());
    const committed = () => el.querySelectorAll(':scope > .chat-message:not(.optimistic)');
    // Self-heal: if our tracked turns drifted from what's actually in the DOM
    // (an earlier early-return, a caught exception, or a race), rebuild from
    // scratch. The old loadTranscript was stateless and so always matched the
    // server; this keeps the incremental path from silently losing an update.
    if (renderedTurns.length !== committed().length) {
      committed().forEach(n => n.remove());
      renderedTurns = [];
    }
    if (!turns.length) {
      renderedTurns.forEach(r => r.node.remove());
      renderedTurns = [];
      const empty = document.createElement('div');
      empty.className = 'chat-loading muted';
      empty.style.padding = '0.5rem';
      empty.textContent = 'no past turns yet';
      const sendAnchor = el.querySelector(':scope > .send-anchor');
      if (sendAnchor) el.insertBefore(empty, sendAnchor);
      else el.appendChild(empty);
      lastTranscriptSig = sig;
      updateLoadEarlier(id);
      return;
    }
    // Reconcile against what's already rendered. Transcripts are append-only,
    // so the new list shares a prefix with the old; keep that prefix's DOM
    // untouched and only append (or, if the tail diverged, replace the tail).
    const newKeys = turns.map(turnKey);
    let lcp = 0;
    while (lcp < renderedTurns.length && lcp < newKeys.length && renderedTurns[lcp].key === newKeys[lcp]) lcp++;
    // Drop the diverged tail (last turn finalized, or the window slid past the
    // front), then append everything past the common prefix.
    for (let i = lcp; i < renderedTurns.length; i++) renderedTurns[i].node.remove();
    renderedTurns.length = lcp;
    suppressAppendScroll = true;
    try {
      for (let i = lcp; i < turns.length; i++) {
        const node = appendChatMessage(turns[i]);
        if (node) renderedTurns.push({ key: newKeys[i], node });
      }
    } finally {
      suppressAppendScroll = false; // never leave it stuck, or future appends won't scroll
    }
    if (opts.anchorHeight != null) {
      // "Load earlier" prepended older turns above the viewport: restore the
      // prior position by the height the prepended content added, so the reader
      // stays on what they were reading.
      el.scrollTop = opts.anchorTop + (el.scrollHeight - opts.anchorHeight);
    } else if (wasAtBottom) {
      // Only follow new turns to the bottom if the reader was already there.
      el.scrollTop = el.scrollHeight;
    }
    // Mark this state rendered only now — a successful render — so any earlier
    // bail-out leaves the signature stale and the next call retries.
    lastTranscriptSig = sig;
    updateLoadEarlier(id);
  } catch {/* ignore — lastTranscriptSig stays put, so the next call retries */}
}

// updateLoadEarlier shows a "load earlier" control at the top of the transcript
// when the server holds older turns beyond the current window, and removes it
// once the whole history is loaded. Disabled mid-turn (the window is shifting).
function updateLoadEarlier(id) {
  const el = document.getElementById('chat-scroll');
  if (!el) return;
  let btn = el.querySelector(':scope > .load-earlier');
  const more = transcriptTotal > renderedTurns.length;
  if (!more) { if (btn) btn.remove(); return; }
  if (!btn) {
    btn = document.createElement('button');
    btn.className = 'load-earlier';
    btn.type = 'button';
    btn.addEventListener('click', () => loadEarlier(id));
    el.insertBefore(btn, el.firstChild);
  } else if (el.firstChild !== btn) {
    el.insertBefore(btn, el.firstChild);
  }
  btn.disabled = detailStreaming;
  btn.textContent = '↑ load earlier (' + renderedTurns.length + '/' + transcriptTotal + ')';
}

// loadEarlier grows the window by a page and re-fetches, anchoring the scroll so
// the prepended history doesn't yank the reader. No-op while a turn streams.
async function loadEarlier(id) {
  if (detailStreaming) return;
  const el = document.getElementById('chat-scroll');
  if (!el) return;
  transcriptLimit += TRANSCRIPT_PAGE;
  const anchorTop = el.scrollTop;
  const anchorHeight = el.scrollHeight;
  lastTranscriptSig = ''; // window changed — force the reconcile past the gate
  await loadTranscript(id, { anchorTop, anchorHeight });
}

// turnKey identifies a transcript turn for incremental reconcile. Append-only
// turns are stable, so role + timestamp + a content fingerprint (length plus a
// short prefix) is enough to spot the divergence point cheaply, without
// stringifying every turn's (potentially 32KB) body on each reconcile.
function turnKey(t) {
  const c = t.content || '';
  return (t.role || '') + '' + (t.ts || '') + '' + c.length + '' + c.slice(0, 48);
}

async function loadChatMessages(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/messages');
    if (!res.ok) return;
    const data = (await res.json()) || [];
    const list = document.getElementById('chat-scroll');
    if (!list) return;
    list.querySelectorAll(':scope > .chat-loading, :scope > .chat-message').forEach(n => n.remove());
    suppressAppendScroll = true;
    for (const m of data) appendChatMessage(m);
    suppressAppendScroll = false;
    list.scrollTop = list.scrollHeight; // fresh main-chat load lands at the bottom
  } catch { suppressAppendScroll = false; }
}

async function loadMainChatInfo(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id));
    if (!res.ok) return;
    const info = await res.json();
    renderFocus(info.focus);
  } catch {}
}

function renderFocus(focus) {
  const left = `<span class="subtitle-left"><strong class="session-title">main chat</strong></span>`;
  if (!focus || !focus.session_id) {
    subtitle.innerHTML = left;
    return;
  }
  const sid = esc(focus.session_id.slice(0, 8));
  subtitle.innerHTML =
    left +
    `<a href="#/s/${esc(focus.session_id)}" class="subtitle-focus">focus: ${sid}</a>`;
}

function appendChatMessage(m) {
  const list = document.getElementById('chat-scroll');
  if (!list) return null;
  // Decide stick-to-bottom BEFORE inserting — the insert changes scrollHeight.
  const stick = !suppressAppendScroll && isNearBottom(list);
  const div = document.createElement('div');
  const role = m.role || 'agent';
  div.className = 'chat-message ' + role + (m._placeholder ? ' placeholder' : '');
  // No client-side default: omitting ts shows no stamp. Callers that
  // already have an authoritative time (server messages, client errors)
  // pass it explicitly; the SSE/POST path later fills it in via
  // updateMessageTs once the server confirms the persisted ts.
  const ts = m.ts ? `<span class="ts">${esc(new Date(m.ts).toLocaleString())}</span>` : '';
  div.innerHTML =
    `<div class="role">${esc(role)}${ts}</div>` +
    `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>`;
  // send-anchor lives inside chat-scroll (sticky at bottom). Insert
  // new messages before it so it stays the last child.
  const sendAnchor = list.querySelector(':scope > .send-anchor');
  if (sendAnchor) list.insertBefore(div, sendAnchor);
  else list.appendChild(div);
  if (stick) list.scrollTop = list.scrollHeight;
  return div;
}

// updateMessageTs sets (or inserts) the timestamp span on an existing
// chat-message node. Used to canonicalize optimistic messages once the
// server returns the persisted ts.
function updateMessageTs(node, ts) {
  if (!node || !ts) return;
  const roleEl = node.querySelector('.role');
  if (!roleEl) return;
  let span = roleEl.querySelector('.ts');
  if (!span) {
    span = document.createElement('span');
    span.className = 'ts';
    roleEl.appendChild(span);
  }
  span.textContent = new Date(ts).toLocaleString();
}

// ---------- Permission-request modal (global, runs in all views) ----------

let pendingInteractions = [];

async function pollInteractions() {
  try {
    const res = await fetch('/api/interactions');
    if (!res.ok) return;
    const list = (await res.json()) || [];
    if (!sameInteractions(pendingInteractions, list)) {
      pendingInteractions = list;
      renderInteractions();
    }
  } catch {/* server may be down briefly */}
}

function sameInteractions(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i].id !== b[i].id) return false;
  return true;
}

// AskUserQuestion surfaces as a pending interaction whose tool_input carries
// the questions + options. We render it as a choice picker (single-select per
// question) instead of allow/deny; the picked labels go back as `answers`,
// which the server feeds into the tool's updatedInput so claude resolves it
// without ever rendering its pane TUI selector. (multiSelect / free-text are
// not handled yet — only the listed options.)
function isAskQuestion(p) {
  return p.tool_name === 'AskUserQuestion'
    && p.tool_input && Array.isArray(p.tool_input.questions) && p.tool_input.questions.length > 0;
}

// previewBlock renders an option's optional `preview` as raw monospace text;
// a tall preview just scrolls inside its box. Deliberately tiny — `preview` is
// a rare field and not worth more than this.
function previewBlock(preview) {
  return preview ? `<span class="qopt-preview">${esc(preview)}</span>` : '';
}

function renderAskQuestion(p, sid) {
  const blocks = p.tool_input.questions.map((q, qi) => {
    const opts = (q.options || []).map((o, oi) => `
      <button class="qopt" data-qi="${qi}" data-oi="${oi}">
        <span class="qopt-label">${esc(o.label || '')}</span>
        ${o.description ? `<span class="qopt-desc">${esc(o.description)}</span>` : ''}
        ${previewBlock(o.preview)}
      </button>`).join('');
    return `
      <div class="question">
        ${q.header ? `<div class="q-header">${esc(q.header)}</div>` : ''}
        <div class="q-text">${esc(q.question || '')}${q.multiSelect ? ' <span class="q-multi">(select all that apply)</span>' : ''}</div>
        <div class="q-options">${opts}</div>
        <input class="qother" data-qi="${qi}" type="text" placeholder="or type your own answer…">
      </div>`;
  }).join('');
  return `
    <div class="interaction ask" data-id="${esc(p.id)}">
      <div class="meta"><strong>question</strong><span class="muted">session ${esc(sid)}</span></div>
      ${blocks}
      <div class="actions">
        <button class="qignore">ignore</button>
        <button class="qsubmit" disabled>answer</button>
      </div>
    </div>`;
}

function renderPermission(p, sid) {
  let inputJSON = '';
  try { inputJSON = JSON.stringify(p.tool_input || {}, null, 2); }
  catch { inputJSON = String(p.tool_input || ''); }
  const matcher = deriveMatcherPreview(p.tool_name, p.tool_input);
  return `
    <div class="interaction" data-id="${esc(p.id)}">
      <div class="meta">
        <strong>${esc(p.tool_name || p.event)}</strong>
        <span class="muted">session ${esc(sid)}</span>
      </div>
      <pre class="tool-input">${esc(inputJSON)}</pre>
      <div class="actions">
        <button class="allow primary" data-scope="once">allow</button>
        <button class="allow secondary" data-scope="session" title="auto-allow ${esc(matcher)} for this session">allow always</button>
        <button class="deny secondary" data-scope="session" title="auto-deny ${esc(matcher)} for this session">deny always</button>
        <button class="deny primary" data-scope="once">deny</button>
      </div>
    </div>`;
}

// wireAskQuestion drives the choice picker. Each question takes an answer that
// is one of: a listed option, several options (when multiSelect), or free text
// (the always-available "Other", matching the native tool) — picking options
// clears the free text and vice-versa. multiSelect picks are joined with ", "
// (the format the native tool emits). The answer button enables once every
// question has an answer; "ignore" denies the tool, which claude treats as
// "skip the question, continue in chat". Answers go back as question -> string
// in one response.
function wireAskQuestion(node, id) {
  const p = pendingInteractions.find(x => x.id === id);
  if (!p) return;
  const qs = p.tool_input.questions;
  const submit = node.querySelector('.qsubmit');
  const otherOf = qi => node.querySelector(`.qother[data-qi="${qi}"]`);
  const answerOf = qi => {
    const typed = otherOf(qi).value.trim();
    if (typed) return typed;
    // Join all selected labels — a single-select question has at most one.
    return [...node.querySelectorAll(`.qopt.selected[data-qi="${qi}"]`)]
      .map(s => (qs[+qi].options[+s.dataset.oi] || {}).label || '').join(', ');
  };
  const recompute = () => {
    submit.disabled = qs.some((_, qi) => !answerOf(qi));
  };
  node.querySelectorAll('.qopt').forEach(btn => {
    btn.addEventListener('click', () => {
      const qi = btn.dataset.qi;
      if (qs[+qi].multiSelect) {
        btn.classList.toggle('selected'); // multi: toggle, leave others
      } else {
        node.querySelectorAll(`.qopt[data-qi="${qi}"]`).forEach(b => b.classList.remove('selected'));
        btn.classList.add('selected'); // single: radio
      }
      otherOf(qi).value = ''; // picking an option clears its free-text
      recompute();
    });
  });
  node.querySelectorAll('.qother').forEach(inp => {
    inp.addEventListener('input', () => {
      if (inp.value.trim()) { // typing clears the radio selection for that question
        node.querySelectorAll(`.qopt[data-qi="${inp.dataset.qi}"]`).forEach(b => b.classList.remove('selected'));
      }
      recompute();
    });
  });
  node.querySelector('.qignore').addEventListener('click',
    () => respond(id, 'deny', 'once', 'The user declined to answer; continue in the conversation.'));
  submit.addEventListener('click', () => {
    const answers = {};
    qs.forEach((q, qi) => { answers[q.question] = answerOf(qi); });
    respondAnswers(id, answers);
  });
}

function renderInteractions() {
  let modal = document.getElementById('modal');
  if (!pendingInteractions.length) {
    if (modal) modal.remove();
    return;
  }
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'modal';
    document.body.appendChild(modal);
  }
  const items = pendingInteractions.map(p => {
    const sid = (p.session_id || '').slice(0, 8) || '(unknown)';
    return isAskQuestion(p) ? renderAskQuestion(p, sid) : renderPermission(p, sid);
  }).join('');
  modal.innerHTML = `
    <div class="overlay"></div>
    <div class="dialog">
      <h3>pending requests (${pendingInteractions.length})</h3>
      ${items}
    </div>
  `;
  modal.querySelectorAll('.interaction').forEach(node => {
    const id = node.dataset.id;
    if (node.classList.contains('ask')) {
      wireAskQuestion(node, id);
      return;
    }
    node.querySelectorAll('button.allow,button.deny').forEach(btn => {
      btn.addEventListener('click', () => {
        const behavior = btn.classList.contains('allow') ? 'allow' : 'deny';
        const scope = btn.dataset.scope || 'once';
        respond(id, behavior, scope);
      });
    });
  });
}

async function respond(id, behavior, scope, reason) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior, scope: scope || 'once', reason: reason || 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond', e);
  }
  pollInteractions();
}

// respondAnswers resolves an AskUserQuestion interaction: behavior "allow"
// plus the chosen labels (question → label), which the server merges into the
// tool's updatedInput.
async function respondAnswers(id, answers) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior: 'allow', answers, reason: 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond answers', e);
  }
  pollInteractions();
}

// Mirror of internal/hook/hook.go deriveMatcher — used purely for the
// tooltip preview ("auto-allow Bash(git:*)" etc). Server is authoritative.
function deriveMatcherPreview(toolName, toolInput) {
  if (!toolName) return '(unknown)';
  if (toolName === 'Bash' && toolInput && typeof toolInput.command === 'string') {
    const cmd = toolInput.command.trim();
    if (cmd) return 'Bash(' + cmd.split(/\s+/, 1)[0] + ':*)';
  }
  return toolName;
}

setInterval(pollInteractions, 2000);
pollInteractions();

setInterval(loadSidebar, 5000);
loadSidebar();

route();

// PWA: register the service worker (installable + offline shell; caching
// strategy and the /api + SSE bypass live in sw.js). Best-effort — a failed
// registration just means no offline/install.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err) =>
      console.warn('service worker registration failed', err));
  });
}
