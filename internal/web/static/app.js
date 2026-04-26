// usher SPA: hash-based routing between session list and detail view.
// Detail view streams subprocess events via SSE.

const root = document.getElementById('root');
const subtitle = document.getElementById('subtitle');

let listInterval = null;
let currentES = null;

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
}

// renderMarkdown turns assistant/user content into safe HTML using the
// vendored snarkdown. Two things keep this safe:
//
//   1. snarkdown does NOT escape ordinary text — any raw `<script>` in the
//      input would otherwise pass through. We HTML-escape the input first,
//      then re-allow `>` so blockquote `> ` still parses (a stray `>` can't
//      form an opening tag because `<` stays escaped).
//
//   2. snarkdown happily emits `<a href="javascript:…">` from user-supplied
//      markdown. We strip risky URL schemes from any anchor / image tag
//      it emits before handing the HTML to the DOM.
//
// Newlines follow Markdown convention: single \n = space, double \n+ =
// soft line break. We tried preprocessing single \n → soft break for chat
// ergonomics and it silently corrupted fenced code blocks and lists, so
// we left it alone. Users who want a break in chat hit Enter twice.
function renderMarkdown(md) {
  let s = String(md || '');
  s = s.replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
  s = s.replace(/&gt;/g, '>');
  let html = window.snarkdown(s);
  // Block javascript:/data:/vbscript: in href / src of snarkdown output.
  html = html.replace(
    /(<(?:a|img)\b[^>]*?\b(?:href|src)=")\s*(?:javascript|data|vbscript)\s*:[^"]*"/gi,
    '$1#unsafe"',
  );
  html = html.replace(/<a /g, '<a target="_blank" rel="noopener" ');
  return html;
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
  } else if (hash === '#/chat' || hash.startsWith('#/chat/')) {
    const id = hash === '#/chat' ? 'default' : decodeURIComponent(hash.slice('#/chat/'.length));
    showMainChat(id);
  } else if (hash.startsWith('#/s/')) {
    showDetail(decodeURIComponent(hash.slice(4)));
  }
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

  root.innerHTML = `
    <div class="session-meta">
      <div class="title">${esc(sess.title || '(untitled)')}</div>
      <div class="cwd">${esc(sess.cwd || '')}</div>
      <div class="id">id: ${esc(sess.id)}</div>
    </div>
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
    <section>
      <h3>send</h3>
      <div class="input-row">
        <textarea id="prompt" placeholder="message…"></textarea>
        <button id="send">send</button>
        <button id="cancel" class="cancel" hidden>cancel</button>
      </div>
      <div class="kbd-hint">Ctrl/Cmd + Enter to send</div>
    </section>
  `;

  await loadTranscript(id);

  const responseEl = document.getElementById('response');
  const eventsEl = document.getElementById('events');
  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const cancelBtn = document.getElementById('cancel');

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
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
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
    <div id="chat-scroll" class="chat-scroll"></div>
    <section>
      <div class="input-row">
        <textarea id="prompt" placeholder="message… (try /help)"></textarea>
        <button id="send">send</button>
      </div>
      <div class="kbd-hint">Ctrl/Cmd + Enter to send · /help for commands</div>
    </section>
  `;

  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');

  await loadChatMessages(id);

  const submit = async () => {
    const text = promptEl.value;
    if (!text.trim() || sendBtn.disabled) return;
    sendBtn.disabled = true;
    promptEl.value = '';
    try {
      const res = await fetch('/api/mainchats/' + encodeURIComponent(id) + '/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      const data = await res.json();
      if (!res.ok) {
        appendChatMessage({ role: 'error', content: data.error || 'send failed' });
      } else {
        for (const m of (data.messages || [])) appendChatMessage(m);
      }
    } catch (e) {
      appendChatMessage({ role: 'error', content: String(e) });
    } finally {
      sendBtn.disabled = false;
      promptEl.focus();
    }
  };

  sendBtn.addEventListener('click', submit);
  promptEl.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
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
        `<div class="content">${renderMarkdown(t.content || '')}</div>`;
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

function appendChatMessage(m) {
  const list = document.getElementById('chat-scroll');
  if (!list) return;
  const div = document.createElement('div');
  div.className = 'chat-message ' + (m.role || 'agent');
  const role = m.role || 'agent';
  div.innerHTML = `<div class="role">${esc(role)}</div><div class="content">${renderMarkdown(m.content || '')}</div>`;
  list.appendChild(div);
  list.scrollTop = list.scrollHeight;
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
    return `
      <div class="interaction" data-id="${esc(p.id)}">
        <div class="meta">
          <strong>${esc(p.tool_name || p.event)}</strong>
          <span class="muted">session ${esc(sid)}</span>
        </div>
        <pre class="tool-input">${esc(inputJSON)}</pre>
        <div class="actions">
          <button class="allow">allow</button>
          <button class="deny">deny</button>
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
    node.querySelector('.allow').addEventListener('click', () => respond(id, 'allow'));
    node.querySelector('.deny').addEventListener('click', () => respond(id, 'deny'));
  });
}

async function respond(id, behavior) {
  try {
    await fetch('/api/interactions/' + encodeURIComponent(id) + '/respond', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ behavior, reason: 'via usher web UI' }),
    });
  } catch (e) {
    console.error('respond', e);
  }
  pollInteractions();
}

setInterval(pollInteractions, 2000);
pollInteractions();

route();
