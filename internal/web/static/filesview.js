// usher SPA: lazy file tree panel on the session detail view.
//
// Read-only directory listing of the session's cwd, served one level at a
// time by GET /api/sessions/{id}/files?dir=<rel> (cwd-fenced, entry-capped,
// names/types only). Per the product boundary this panel never fetches file
// contents: clicking a file only raises a subtle "not available" tooltip.

import { esc } from './state.js';

// One live panel at a time. showDetail re-mounts on every navigation and
// replaces `view` wholesale; async level fetches check the mount epoch so a
// response landing after navigation can't write into a dead panel.
let view = null;
let toggleWired = false;

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

// Small inline-SVG set, one per extension class (14px strokes, currentColor).
const ICON_DOC = 'M4 1.5h5.5L12 4v9.5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V2.5a1 1 0 0 1 1-1zM9.5 1.5V4H12';
const svg = (inner, w) =>
  `<svg viewBox="0 0 16 16" width="${w || 14}" height="${w || 14}" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${inner}</svg>`;
const ICONS = {
  dirClosed: svg('<path d="M2 4.2a1 1 0 0 1 1-1h3.1L8 5.1h5a1 1 0 0 1 1 1v5.7a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1z"/>'),
  dirOpen: svg('<path d="M2 4.2a1 1 0 0 1 1-1h3.1L8 5.1h5a1 1 0 0 1 1 1v.9"/><path d="M2 12.4 4.1 7.2a1 1 0 0 1 .9-.6h9.2l-2 5.2a1 1 0 0 1-1 .6H3a1 1 0 0 1-1-.9z"/>'),
  file: svg(`<path d="${ICON_DOC}"/>`),
  md: svg(`<path d="${ICON_DOC}"/><path d="M5.5 8.2h5M5.5 10.6h5"/>`),
  sh: svg(`<path d="${ICON_DOC}"/><path d="M5.4 8.2 6.9 9.7 5.4 11.2M8 11.4h2.6"/>`),
  go: svg(`<path d="${ICON_DOC}"/><text x="7.4" y="11.4" font-size="5" text-anchor="middle" fill="currentColor" stroke="none">go</text>`),
  json: svg(`<path d="${ICON_DOC}"/><text x="7.4" y="11.4" font-size="5.5" text-anchor="middle" fill="currentColor" stroke="none">{}</text>`),
  image: svg('<rect x="2" y="3" width="12" height="10" rx="1"/><circle cx="5.6" cy="6.4" r="1.1"/><path d="M2.5 11.5 6 8l2.4 2.4L10.5 8l3 3"/>'),
  reload: svg('<path d="M13.5 8a5.5 5.5 0 1 1-1.6-3.9M13.5 2.2v2.6H10.9"/>', 13),
};

// iconFor picks the extension class for a file entry. Anything that isn't a
// regular file ("other": escaping symlinks, sockets, …) gets the generic icon.
function iconFor(entry) {
  if (entry.type !== 'file') return ICONS.file;
  const name = entry.name;
  const dot = name.lastIndexOf('.');
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : '';
  if (ext === 'md' || ext === 'markdown') return ICONS.md;
  if (ext === 'sh' || ext === 'bash' || ext === 'zsh') return ICONS.sh;
  if (ext === 'go') return ICONS.go;
  if (ext === 'json') return ICONS.json;
  if (ext === 'png' || ext === 'jpg' || ext === 'jpeg' || ext === 'gif' || ext === 'webp' || ext === 'svg' || ext === 'ico') return ICONS.image;
  return ICONS.file;
}

// setupFilesPanel mounts the panel into the detail view's #files-panel aside
// and reveals the persistent header toggle. Called once per showDetail mount.
export function setupFilesPanel(id, cwd) {
  const panel = document.getElementById('files-panel');
  const toggle = document.getElementById('files-toggle');
  if (!panel) return;
  if (view && view.ro) view.ro.disconnect();
  const epoch = (view ? view.epoch : 0) + 1;

  // Root path header: directory portion greyed, last segment full ink.
  const clean = (cwd || '').replace(/\/+$/, '');
  const idx = clean.lastIndexOf('/');
  const dirPart = idx >= 0 ? clean.slice(0, idx + 1) : '';
  const basePart = clean ? (idx >= 0 ? clean.slice(idx + 1) : clean) : '/';
  panel.innerHTML = `
    <div class="files-head">
      <span class="files-root" title="${esc(cwd || '')}"><span class="files-root-dir">${esc(dirPart)}</span><span class="files-root-base">${esc(basePart)}</span></span>
      <button type="button" class="files-iconbtn" title="reload file list" aria-label="reload file list">${ICONS.reload}</button>
    </div>
    <div class="files-tree"></div>
  `;

  view = {
    epoch,
    id,
    panel,
    treeEl: panel.querySelector('.files-tree'),
    rootEl: panel.querySelector('.files-root'),
    expanded: new Set(), // relative dir paths; survives reload, not navigation
    levels: new Map(),   // path → {status:'loading'} | {status:'error', error} | {status:'ready', entries, truncated}
    lastTreeHTML: '',
    tipTimer: 0,
    ro: null,
  };

  view.treeEl.addEventListener('click', (e) => {
    const dir = e.target.closest('.files-dir');
    if (dir) { toggleDir(dir.dataset.path); return; }
    const file = e.target.closest('.files-file');
    if (file) showFileTip(file);
  });
  panel.querySelector('.files-iconbtn').addEventListener('click', reload);

  // Fade the clipped START of the root path when it overflows. The element
  // is scrolled to its end (scrollLeft clamps even with overflow:hidden), so
  // the tail — the meaningful part of the path — stays visible.
  const measureRootClip = () => {
    const v = view;
    if (!v || !v.rootEl || v.rootEl.closest('.files-panel').hidden) return;
    const el = v.rootEl;
    el.scrollLeft = el.scrollWidth;
    if (el.scrollWidth > el.clientWidth + 1) el.dataset.clipped = 'start';
    else delete el.dataset.clipped;
  };
  if (typeof ResizeObserver !== 'undefined') {
    view.ro = new ResizeObserver(measureRootClip);
    view.ro.observe(view.rootEl);
  }

  if (toggle) {
    toggle.hidden = false;
    if (!toggleWired) {
      toggleWired = true;
      toggle.addEventListener('click', () => {
        setOpen(toggle.getAttribute('aria-pressed') !== 'true');
      });
    }
  }
  let open = false;
  try { open = localStorage.getItem('usher.files.open') === '1'; } catch {/* private mode */}
  setOpen(open);
}

// setOpen shows/hides the panel and persists the choice across sessions.
function setOpen(open) {
  const toggle = document.getElementById('files-toggle');
  try { localStorage.setItem('usher.files.open', open ? '1' : '0'); } catch {/* private mode */}
  if (toggle) toggle.setAttribute('aria-pressed', open ? 'true' : 'false');
  const v = view;
  if (!v) return;
  v.panel.hidden = !open;
  if (open && !v.levels.size) fetchLevel('');
}

// toggleDir expands a directory (fetching its level lazily on first expand)
// or collapses it. Collapsing keeps the loaded level, so a re-expand is
// instant; re-expanding a failed level retries the fetch.
function toggleDir(path) {
  const v = view;
  if (!v) return;
  if (v.expanded.has(path)) {
    v.expanded.delete(path);
    renderTree();
    return;
  }
  v.expanded.add(path);
  const lvl = v.levels.get(path);
  if (lvl && lvl.status === 'ready') renderTree();
  else fetchLevel(path);
}

// reload drops every fetched level but keeps the expanded set, then re-fetches
// root plus each expanded directory in parallel.
function reload() {
  const v = view;
  if (!v) return;
  v.levels = new Map();
  v.lastTreeHTML = '';
  fetchLevel('');
  for (const p of v.expanded) fetchLevel(p);
}

async function fetchLevel(path) {
  const v = view;
  if (!v) return;
  const epoch = v.epoch;
  v.levels.set(path, { status: 'loading' });
  renderTree();
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(v.id) + '/files?dir=' + encodeURIComponent(path));
    const body = await res.json().catch(() => ({}));
    if (!view || view.epoch !== epoch) return;
    if (!res.ok) {
      // The server's 400 reason ("not a directory", "outside workspace", …)
      // is shown plainly as the level's content.
      view.levels.set(path, { status: 'error', error: body.error || ('HTTP ' + res.status) });
    } else {
      const entries = (body.entries || []).slice().sort((a, b) => {
        const ad = a.type === 'directory' ? 0 : 1;
        const bd = b.type === 'directory' ? 0 : 1;
        return ad - bd || collator.compare(a.name, b.name);
      });
      view.levels.set(path, { status: 'ready', entries, truncated: !!body.truncated });
    }
  } catch (e) {
    if (!view || view.epoch !== epoch) return;
    view.levels.set(path, { status: 'error', error: String((e && e.message) || e) });
  }
  renderTree();
}

function renderTree() {
  const v = view;
  if (!v || !v.treeEl.isConnected) return;
  const html = levelHTML('', 0);
  // innerHTML cache: skip the write when nothing changed (rebuilds restart
  // CSS transitions and reset hover states for no reason).
  if (html === v.lastTreeHTML) return;
  v.lastTreeHTML = html;
  v.treeEl.innerHTML = html;
}

function levelHTML(path, depth) {
  const v = view;
  const lvl = v.levels.get(path);
  if (!lvl || lvl.status === 'loading') {
    return `<div class="files-note" style="--depth:${depth}">loading…</div>`;
  }
  if (lvl.status === 'error') {
    return `<div class="files-note files-error" style="--depth:${depth}">${esc(lvl.error)}</div>`;
  }
  let html = lvl.entries.map(e => rowHTML(e, path, depth)).join('');
  if (!lvl.entries.length) {
    html = `<div class="files-note" style="--depth:${depth}">empty</div>`;
  }
  if (lvl.truncated) {
    html += `<div class="files-note" style="--depth:${depth}">… truncated</div>`;
  }
  return html;
}

function rowHTML(entry, parentPath, depth) {
  const v = view;
  const childPath = parentPath ? parentPath + '/' + entry.name : entry.name;
  if (entry.type === 'directory') {
    const open = v.expanded.has(childPath);
    let html = `<button type="button" class="files-row files-dir" data-path="${esc(childPath)}" aria-expanded="${open ? 'true' : 'false'}" style="--depth:${depth}">` +
      (open ? ICONS.dirOpen : ICONS.dirClosed) +
      `<span class="files-name">${esc(entry.name)}</span></button>`;
    if (open) html += levelHTML(childPath, depth + 1);
    return html;
  }
  return `<button type="button" class="files-row files-file" style="--depth:${depth}">` +
    iconFor(entry) +
    `<span class="files-name">${esc(entry.name)}</span></button>`;
}

// showFileTip is the entire file-click behavior in v1: a subtle, self-dismissing
// "not available" tooltip. No file contents ever leave the server (boundary).
function showFileTip(row) {
  const v = view;
  if (!v) return;
  let tip = v.panel.querySelector('.files-tip');
  if (!tip) {
    tip = document.createElement('div');
    tip.className = 'files-tip';
    tip.setAttribute('role', 'status');
    v.panel.appendChild(tip);
  }
  tip.textContent = 'file preview not available';
  const pr = v.panel.getBoundingClientRect();
  const rr = row.getBoundingClientRect();
  tip.style.top = Math.max(0, rr.bottom - pr.top + 2) + 'px';
  tip.style.left = Math.min(rr.left - pr.left + 8, Math.max(0, pr.width - 160)) + 'px';
  tip.classList.add('show');
  clearTimeout(v.tipTimer);
  v.tipTimer = setTimeout(() => tip.classList.remove('show'), 1600);
}

// The header toggle is persistent chrome (index.html) but only meaningful on
// session detail routes; hide it everywhere else. The detail mount re-shows
// it via setupFilesPanel (hashchange fires before the route re-renders).
window.addEventListener('hashchange', () => {
  if (location.hash.startsWith('#/s/')) return;
  const toggle = document.getElementById('files-toggle');
  if (toggle) toggle.hidden = true;
  if (view && view.ro) view.ro.disconnect();
  view = null;
});
