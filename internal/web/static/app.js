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
// Detail-view transcript sync. The persistent SSE renders only turns this
// client witnessed from subprocess.started, so a turn that began before the
// view opened (or during an SSE reconnect gap) is missed — we re-fetch the
// transcript when a turn ends and on SSE reconnect. detailStreaming gates the
// reconnect re-fetch (don't clobber a live bubble), lastTranscriptSig skips the
// rebuild when nothing changed, and currentDetailId guards a late async
// re-fetch from rendering into a view the user already navigated away from.
let detailStreaming = false;
let lastTranscriptSig = '';
let currentDetailId = null;
// Last sidebar HTML written to the DOM. The sidebar re-polls every 5s; skipping
// the innerHTML rewrite when nothing changed keeps the live-dot CSS animation
// from restarting (jumping back to its bright peak) on every poll.
let lastSidebarHtml = '';
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
  if (!listInterval) listInterval = setInterval(loadList, 5000);
  await loadList();
}

async function loadList() {
  if (location.hash && location.hash !== '#/' && location.hash !== '') return;
  try {
    const res = await fetch('/api/sessions');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = (await res.json()) || [];
    if (!data.length) {
      root.innerHTML = '<div class="empty">no sessions found</div>';
      return;
    }
    const rows = data.map(s => {
      const status = s.status === 'running'
        ? '<span class="running-dot executing" title="executing">●</span> running'
        : (s.status === 'live'
          ? '<span class="running-dot" title="process live">●</span> live'
          : '');
      return `
      <tr data-id="${esc(s.id)}">
        <td class="title">${esc(s.title || '(untitled)')} ${status}</td>
        <td class="cwd">${esc(s.cwd || '')}</td>
        <td>${esc(fmt(s.last_event_at))}</td>
        <td class="id">${esc(s.id.slice(0, 8))}</td>
      </tr>
    `;
    }).join('');
    root.innerHTML = `<table>
      <thead><tr><th>title</th><th>cwd</th><th>last activity</th><th>id</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
    root.querySelectorAll('tbody tr').forEach(tr => {
      tr.addEventListener('click', () => {
        location.hash = '#/s/' + encodeURIComponent(tr.dataset.id);
      });
    });
  } catch (e) {
    root.innerHTML = '<div class="err">failed to load: ' + esc(String(e)) + '</div>';
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
    // Optimistic: show user message immediately. Assistant placeholder is
    // created by openEventStream on subprocess.started.
    turnState.userNode = appendChatMessage({ role: 'user', content: text });
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

  const onIdle = () => {
    detailStreaming = false;
    sendBtn.disabled = false;
    if (cancelBtn) cancelBtn.hidden = true;
  };
  const onRunning = () => {
    detailStreaming = true;
    sendBtn.disabled = true;
    if (cancelBtn) cancelBtn.hidden = false;
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
      onRunning();
    },
    'assistant': (d) => {
      // Message granularity: each assistant turn carries its full text blocks
      // (no token deltas). A turn may produce several assistant messages (text
      // before a tool call, then more after); accumulate their text into the
      // one placeholder. Tool-only messages have no text and are skipped.
      if (!placeholder) return;
      const text = assistantText(d);
      if (!text) return;
      accum += (accum ? '\n' : '') + text;
      setContent(placeholder, accum);
      chatEl.scrollTop = chatEl.scrollHeight;
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
        placeholder.className = 'chat-message error';
        setRoleText(placeholder, 'error');
        setContent(placeholder, msg);
        placeholder = null;
      } else {
        appendChatMessage({ role: 'error', content: msg, ts: new Date().toISOString() });
      }
      onIdle();
    },
    // Dropped (diagnostic noise): 'user' (our own prompt / tool results),
    // 'system' init/status, and other jsonl bookkeeping lines.
  };

  Object.entries(handlers).forEach(([name, fn]) => {
    es.addEventListener(name, (ev) => {
      let data;
      try { data = JSON.parse(ev.data); } catch { data = { raw: ev.data }; }
      try { fn(data); } catch (e) { console.error('handler error', name, e); }
    });
  });

  es.onerror = () => {/* SSE auto-reconnects; no user-visible noise */};
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

async function loadTranscript(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/transcript?limit=100');
    if (!res.ok) return;
    const turns = (await res.json()) || [];
    // A late re-fetch must not render into a view the user already left.
    if (id !== currentDetailId) return;
    // Transcripts are append-only, so a change shows up as a longer list or a
    // mutated last turn. Skip the rebuild when nothing changed (no flicker /
    // scroll yank when there's nothing new).
    const last = turns[turns.length - 1];
    const sig = turns.length + ':' + (last ? JSON.stringify(last) : '');
    if (sig === lastTranscriptSig) return;
    lastTranscriptSig = sig;
    const el = document.getElementById('chat-scroll');
    if (!el) return;
    // Wipe transient prior content (loading stub + any messages) but
    // preserve the send-anchor child.
    el.querySelectorAll(':scope > .chat-loading, :scope > .chat-message').forEach(n => n.remove());
    if (!turns.length) {
      const empty = document.createElement('div');
      empty.className = 'chat-loading muted';
      empty.style.padding = '0.5rem';
      empty.textContent = 'no past turns yet';
      const sendAnchor = el.querySelector(':scope > .send-anchor');
      if (sendAnchor) el.insertBefore(empty, sendAnchor);
      else el.appendChild(empty);
      return;
    }
    for (const t of turns) appendChatMessage(t);
  } catch {/* ignore */}
}

async function loadChatMessages(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/messages');
    if (!res.ok) return;
    const data = (await res.json()) || [];
    const list = document.getElementById('chat-scroll');
    if (!list) return;
    list.querySelectorAll(':scope > .chat-loading, :scope > .chat-message').forEach(n => n.remove());
    for (const m of data) appendChatMessage(m);
  } catch {}
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
  list.scrollTop = list.scrollHeight;
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
