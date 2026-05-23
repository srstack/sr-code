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
let renderMode = localStorage.getItem('usher.renderMode') === 'raw' ? 'raw' : 'md';

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
  renderModeBtn.textContent = 'render: ' + renderMode;
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
    const res = await fetch('/api/sessions');
    const sessions = res.ok ? (await res.json() || []) : [];
    renderSidebarSessions(sessions);
    updateSidebarActive();
  } catch {/* server may be down briefly */}
}

function renderSidebarSessions(sessions) {
  const wrap = document.getElementById('sidebar-sessions');
  const count = document.getElementById('sidebar-session-count');
  if (count) count.textContent = '(' + sessions.length + ')';
  if (!wrap) return;
  if (!sessions.length) {
    wrap.innerHTML = '<div class="sidebar-empty">no sessions found</div>';
    return;
  }
  const groups = new Map();
  for (const s of sessions) {
    const cwd = s.cwd || '(unknown)';
    if (!groups.has(cwd)) groups.set(cwd, []);
    groups.get(cwd).push(s);
  }
  const recencyOf = arr => Math.max(...arr.map(s => Date.parse(s.last_event_at) || 0));
  const cwds = [...groups.keys()].sort((a, b) => recencyOf(groups.get(b)) - recencyOf(groups.get(a)));
  wrap.innerHTML = cwds.map(cwd => {
    const items = groups.get(cwd).slice().sort((a, b) =>
      (Date.parse(b.last_event_at) || 0) - (Date.parse(a.last_event_at) || 0)
    );
    const lis = items.map(s => {
      const href = '#/s/' + encodeURIComponent(s.id);
      const dot = s.status === 'running'
        ? '<span class="running-dot" title="active subprocess">●</span>'
        : '';
      const auto = s.auto_approve
        ? '<span class="auto-dot" title="auto-approve enabled">⚡</span>'
        : '';
      const title = s.title || '(untitled)';
      return `<li><a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${dot}${auto}${esc(title)}</a></li>`;
    }).join('');
    return `<div class="cwd-group">
      <div class="cwd-label" title="${esc(cwd)}">${esc(cwd)} (${items.length})</div>
      <ul class="sidebar-list">${lis}</ul>
    </div>`;
  }).join('');
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
        ? '<span class="running-dot" title="active subprocess">● running</span>'
        : '';
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
  subtitle.innerHTML =
    `<strong class="session-title">${esc(sess.title || '(untitled)')}</strong>` +
    ` <span class="session-cwd">${esc(sess.cwd || '')}</span>` +
    ` <span class="session-id">${esc(sess.id.slice(0, 8))}</span>`;

  root.innerHTML = `
    <section>
      <h3>transcript</h3>
      <div id="transcript" class="chat-scroll"><div class="muted" style="padding:0.5rem">loading…</div></div>
    </section>
    <section>
      <h3>response (current send)</h3>
      <div id="response" class="content"></div>
    </section>
    <section>
      <h3>events</h3>
      <ul id="events"></ul>
    </section>
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
  `;

  await loadTranscript(id);

  const responseEl = document.getElementById('response');
  const eventsEl = document.getElementById('events');
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
        addEvent(eventsEl, 'error', 'auto-approve toggle failed: ' + String(e));
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
      addEvent(eventsEl, 'error', 'cancel failed: ' + String(e));
    } finally {
      cancelBtn.disabled = false;
    }
  });

  openEventStream(id, responseEl, eventsEl, sendBtn, cancelBtn);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    sendBtn.textContent = 'sending…';
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        addEvent(eventsEl, 'error', 'send failed: ' + (err.error || ('HTTP ' + res.status)));
        sendBtn.disabled = false;
        sendBtn.textContent = 'send';
        return;
      }
      promptEl.value = '';
      // button stays disabled until subprocess.exit
    } catch (e) {
      addEvent(eventsEl, 'error', String(e));
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
  promptEl.focus();
}

function openEventStream(id, responseEl, eventsEl, sendBtn, cancelBtn) {
  const es = new EventSource('/api/sessions/' + encodeURIComponent(id) + '/events');
  currentES = es;

  let accum = ''; // raw markdown text accumulated from text_delta events

  const onIdle = () => {
    sendBtn.disabled = false;
    sendBtn.textContent = 'send';
    if (cancelBtn) cancelBtn.hidden = true;
  };
  const onRunning = () => {
    sendBtn.disabled = true;
    if (cancelBtn) cancelBtn.hidden = false;
  };

  const handlers = {
    'subprocess.started': (d) => {
      accum = '';
      responseEl.innerHTML = '';
      delete responseEl.dataset.raw;
      addEvent(eventsEl, 'info', `subprocess started (pid ${d.pid})`);
      onRunning();
    },
    'system': (d) => {
      if (d.subtype === 'init') {
        addEvent(eventsEl, 'info', `session init`);
      } else if (d.subtype === 'status') {
        addEvent(eventsEl, 'info', `status: ${d.status}`);
      }
    },
    'stream_event': (d) => {
      const e = d.event;
      if (e && e.type === 'content_block_delta' && e.delta && e.delta.type === 'text_delta') {
        accum += e.delta.text;
        responseEl.dataset.raw = accum;
        responseEl.innerHTML = renderMarkdown(accum);
      }
    },
    'assistant': () => {/* full message; we already streamed via deltas */},
    'result': (d) => {
      addEvent(eventsEl, d.is_error ? 'error' : 'info',
        `result: ${d.is_error ? 'error' : 'success'}` +
        (typeof d.duration_ms === 'number' ? ` · ${d.duration_ms} ms` : ''));
    },
    'rate_limit_event': () => {/* ignore */},
    'subprocess.exit': (d) => {
      addEvent(eventsEl, d.exit_code === 0 ? 'exit' : 'error',
        `subprocess exited (${d.exit_code})` + (d.error ? ` · ${d.error}` : ''));
      onIdle();
      // Refresh transcript so the just-finished turn appears in history.
      loadTranscript(id);
    },
    'error': (d) => {
      addEvent(eventsEl, 'error', d.message || JSON.stringify(d));
      onIdle();
    },
  };

  Object.entries(handlers).forEach(([name, fn]) => {
    es.addEventListener(name, (ev) => {
      let data;
      try { data = JSON.parse(ev.data); } catch { data = { raw: ev.data }; }
      try { fn(data); } catch (e) { console.error('handler error', name, e); }
    });
  });

  es.onerror = () => {
    addEvent(eventsEl, 'error', 'event stream disconnected');
  };
}

function addEvent(eventsEl, kind, text) {
  const li = document.createElement('li');
  li.className = kind;
  li.textContent = `[${timeNow()}] ${text}`;
  eventsEl.appendChild(li);
  eventsEl.scrollTop = eventsEl.scrollHeight;
}

// ---------- Main chat view ----------

async function showMainChat(id) {
  clearListInterval();
  closeES();
  subtitle.textContent = 'main chat · ' + id;

  root.innerHTML = `
    <div id="chat-focus" class="chat-focus muted"></div>
    <div id="chat-scroll" class="chat-scroll"></div>
    <section class="send-anchor">
      <div class="input-row">
        <textarea id="prompt" placeholder="message… (try /help)"></textarea>
        <button id="send">send</button>
      </div>
    </section>
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
    appendChatMessage({ role: 'user', content: text });
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
        appendChatMessage({ role: 'error', content: data.error || 'send failed' });
      } else {
        // Server returns the persisted user+agent pair. We already showed user
        // optimistically, so only render the agent reply (and any extras) here.
        const msgs = (data.messages || []).filter(m => m.role !== 'user');
        for (const m of msgs) appendChatMessage(m);
        // Focus may have shifted this turn — update the header.
        renderFocus(data.focus);
      }
    } catch (e) {
      if (placeholder) placeholder.remove();
      appendChatMessage({ role: 'error', content: String(e) });
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

async function loadTranscript(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/transcript?limit=100');
    if (!res.ok) return;
    const turns = (await res.json()) || [];
    const el = document.getElementById('transcript');
    if (!el) return;
    if (!turns.length) {
      el.innerHTML = '<div class="muted" style="padding:0.5rem">no past turns yet</div>';
      return;
    }
    el.innerHTML = '';
    for (const t of turns) {
      const div = document.createElement('div');
      div.className = 'chat-message ' + (t.role || 'assistant');
      const ts = t.ts ? new Date(t.ts).toLocaleString() : '';
      div.innerHTML =
        `<div class="role">${esc(t.role)}<span class="ts">${esc(ts)}</span></div>` +
        `<div class="content" data-raw="${esc(t.content || '')}">${renderMarkdown(t.content || '')}</div>`;
      el.appendChild(div);
    }
    el.scrollTop = el.scrollHeight;
  } catch {/* ignore */}
}

async function loadChatMessages(id) {
  try {
    const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/messages');
    if (!res.ok) return;
    const data = (await res.json()) || [];
    const list = document.getElementById('chat-scroll');
    list.innerHTML = '';
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
  const el = document.getElementById('chat-focus');
  if (!el) return;
  if (!focus || !focus.session_id) {
    el.textContent = 'no focus session yet';
    el.classList.add('empty');
    return;
  }
  el.classList.remove('empty');
  const sid = (focus.session_id || '').slice(0, 8);
  const cwd = focus.cwd ? ' · ' + focus.cwd : '';
  const title = focus.title ? ' · ' + focus.title : '';
  el.innerHTML = `<a href="#/s/${esc(focus.session_id)}">focus: ${esc(sid)}</a>${esc(cwd)}${esc(title)}`;
}

function appendChatMessage(m) {
  const list = document.getElementById('chat-scroll');
  if (!list) return null;
  const div = document.createElement('div');
  div.className = 'chat-message ' + (m.role || 'agent') + (m._placeholder ? ' placeholder' : '');
  const role = m.role || 'agent';
  div.innerHTML =
    `<div class="role">${esc(role)}</div>` +
    `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>`;
  list.appendChild(div);
  list.scrollTop = list.scrollHeight;
  return div;
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
      </div>
    `;
  }).join('');
  modal.innerHTML = `
    <div class="overlay"></div>
    <div class="dialog">
      <h3>permission requests (${pendingInteractions.length})</h3>
      ${items}
    </div>
  `;
  modal.querySelectorAll('.interaction').forEach(node => {
    const id = node.dataset.id;
    node.querySelectorAll('button.allow,button.deny').forEach(btn => {
      btn.addEventListener('click', () => {
        const behavior = btn.classList.contains('allow') ? 'allow' : 'deny';
        const scope = btn.dataset.scope || 'once';
        respond(id, behavior, scope);
      });
    });
  });
}

async function respond(id, behavior, scope) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior, scope: scope || 'once', reason: 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond', e);
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
