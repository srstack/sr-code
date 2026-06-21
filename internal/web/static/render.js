// usher SPA: markdown + turn rendering + appendChatMessage.

import {
  esc, renderMode, setRenderModeValue, renderModeBtn,
  isNearBottom, suppressAppendScroll,
} from './state.js';

// renderMarkdown turns assistant/user content into safe HTML using the
// vendored marked (GFM: tables, task lists, nested lists, strikethrough,
// autolinks). marked deliberately ships without HTML sanitization, so two
// of our own layers keep things safe:
//
//   1. We let marked do all entity-escaping (it correctly escapes both text
//      and code content) and only neutralize the one thing it would pass
//      through verbatim — raw HTML — by escaping every block/inline `html`
//      token via the renderer hook. A raw `<script>` thus lands as literal
//      text. (The old approach pre-escaped the whole input instead, which
//      double-escaped entities inside code spans/fences because marked
//      re-encodes code content — so `don't` rendered as `don&#39;t`.)
//
//   2. We strip risky URL schemes (javascript:/data:/vbscript:) from any
//      <a>/<img> in marked's output before handing it to the DOM.
//
// breaks:true: a single \n becomes <br> so replies keep their line breaks
// (CommonMark would fold a lone \n to a space). Blank line = new paragraph.
window.marked.use({
  gfm: true,
  breaks: true,
  silent: true,
  renderer: {
    html(token) { return esc(typeof token === 'string' ? token : token.text); },
  },
});

export function renderMarkdown(md) {
  if (renderMode === 'raw') {
    return '<pre class="raw-markdown">' + esc(md || '') + '</pre>';
  }
  let html = window.marked.parse(String(md || ''));
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
export function rerenderAllContent() {
  document.querySelectorAll('[data-raw]').forEach(el => {
    el.innerHTML = renderMarkdown(el.dataset.raw);
  });
}

export function setRenderMode(mode) {
  setRenderModeValue(mode === 'raw' ? 'raw' : 'md');
  localStorage.setItem('usher.renderMode', renderMode);
  updateRenderModeBtn();
  rerenderAllContent();
}

export function updateRenderModeBtn() {
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

// ---------- Rendering turns ----------

// renderToolPart renders a single tool part as a collapsible <details> element.
// Edit/Write expand by default; others collapse.
export function renderToolPart(p) {
  const name = p.toolName || 'tool';
  const target = p.toolTarget || '';
  const expandByDefault = /^(Edit|Write)$/i.test(name);
  const openAttr = expandByDefault ? ' open' : '';
  const label = target ? esc(name) + ' <span class="tool-target">' + esc(target) + '</span>' : esc(name);
  return `<details class="tool-details"${openAttr}>` +
    `<summary>${label}</summary>` +
    `<div class="tool-body" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>` +
    `</details>`;
}

// forkBtnHTML is the fork control at the foot of a completed assistant turn —
// the fork keeps everything above the divider and drops everything below (on
// the last turn: fork the current state). data-uuid is the fork point the
// server expects; clicks are handled by a document-level delegate.
export function forkBtnHTML(uuid) {
  return `<button class="turn-fork" type="button" data-uuid="${esc(uuid)}" title="Fork: branch a new session from this point">⑂ fork</button>`;
}

// renderAssistantParts renders the parts array of a grouped assistant turn.
export function renderAssistantParts(parts) {
  return (parts || []).map(p => {
    if (p.type === 'tool') return renderToolPart(p);
    return `<div class="content" data-raw="${esc(p.content || '')}">${renderMarkdown(p.content || '')}</div>`;
  }).join('');
}

export function appendChatMessage(m) {
  const list = document.getElementById('chat-scroll');
  if (!list) return null;
  // Decide stick-to-bottom BEFORE inserting — the insert changes scrollHeight.
  const stick = !suppressAppendScroll && isNearBottom(list);
  const div = document.createElement('div');
  const role = m.role || 'agent';
  div.className = 'chat-message ' + role + (m._placeholder ? ' placeholder' : '');
  const ts = m.ts ? `<span class="ts">${esc(new Date(m.ts).toLocaleString())}</span>` : '';
  const modelAttr = m.model ? ` title="${esc(m.model)}"` : '';

  if (role === 'assistant' && m.parts) {
    // Grouped assistant turn: role header + structured parts. An EMPTY parts
    // array is the live-turn shell — header only; streamed parts append into
    // it (a flat .content div here would misrender the first tool part).
    // Completed turns close with the fork control at the card's bottom edge.
    div.innerHTML =
      `<div class="role"${modelAttr}>${esc(role)}${ts}</div>` +
      renderAssistantParts(m.parts) +
      (m.uuid ? forkBtnHTML(m.uuid) : '');
  } else {
    // User, error, agent, or streaming placeholder (flat content).
    div.innerHTML =
      `<div class="role"${modelAttr}>${esc(role)}${ts}</div>` +
      `<div class="content" data-raw="${esc(m.content || '')}">${renderMarkdown(m.content || '')}</div>`;
  }
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
export function updateMessageTs(node, ts) {
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

// statusDot renders the sidebar run-state indicator: a dim green dot when
// usher holds a warm-but-idle process ("live"), and a brighter pulsing dot
// while a turn is executing ("running"). Idle/undiscovered sessions get none.
export function statusDot(status) {
  if (status === 'running') return '<span class="running-dot executing" title="executing">●</span>';
  if (status === 'live') return '<span class="running-dot" title="process live">●</span>';
  return '';
}

// Inner SVG for each backend mark (24x24 viewBox; sized/tinted via CSS).
// claude: 10 round-capped spokes from the centre (12,12) out to radius 9.6,
//   tilted 5deg clockwise.
// codex: 6 outward 144deg arcs (radius 3.68) joining hexagon vertices on a
//   radius-7 circle, filled, tilted 15deg clockwise.
export const BACKEND_MARKS = {
  claude: '<g stroke="currentColor" stroke-width="2.1" stroke-linecap="round">'
    + '<line x1="12" y1="12" x2="12.84" y2="2.44"/><line x1="12" y1="12" x2="18.30" y2="4.75"/>'
    + '<line x1="12" y1="12" x2="21.35" y2="9.84"/><line x1="12" y1="12" x2="20.84" y2="15.75"/>'
    + '<line x1="12" y1="12" x2="16.94" y2="20.23"/><line x1="12" y1="12" x2="11.16" y2="21.56"/>'
    + '<line x1="12" y1="12" x2="5.70" y2="19.25"/><line x1="12" y1="12" x2="2.65" y2="14.16"/>'
    + '<line x1="12" y1="12" x2="3.16" y2="8.25"/><line x1="12" y1="12" x2="7.06" y2="3.77"/></g>',
  codex: '<path d="M13.81,5.24A3.680 3.680 0 0 1 18.76,10.19A3.680 3.680 0 0 1 16.95,16.95'
    + 'A3.680 3.680 0 0 1 10.19,18.76A3.680 3.680 0 0 1 5.24,13.81A3.680 3.680 0 0 1 7.05,7.05'
    + 'A3.680 3.680 0 0 1 13.81,5.24Z" fill="currentColor"/>',
};

// Unknown/empty backend falls back to claude, mirroring the router default.
export function backendMark(backend) {
  const b = backend === 'codex' ? 'codex' : 'claude';
  return `<svg class="backend-mark backend-mark--${b}" viewBox="0 0 24 24" aria-hidden="true" focusable="false">${BACKEND_MARKS[b]}</svg>`;
}
