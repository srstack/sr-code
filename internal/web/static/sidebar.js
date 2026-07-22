// usher SPA: sidebar rendering, kebab popover, toggle, settings menu.

import {
  esc, cwdExpanded,
  isUnread, reconcileUnread,
  setLastSessions,
  updateTabBadge,
  registerRenderSidebarSessions,
  refreshSubtitle, currentDetailId,
  editorUrl, pendingPermissionCount,
} from './state.js';
import { statusDot, backendMark } from './render.js';
import { loadList } from './list.js';

// --- sidebar-private state ---
// Last sidebar HTML written to the DOM. The sidebar re-polls every 5s; skipping
// the innerHTML rewrite when nothing changed keeps the live-dot CSS animation
// from restarting (jumping back to its bright peak) on every poll.
let lastSidebarHtml = '';
let lastPinnedHtml = '';
const subagentExpanded = new Set();

// ---------- Sidebar ----------
//
// Polled every 5s independently of the active route. Renders Claude Code
// sessions grouped by cwd, recent activity first. The "main chat" entry is
// static markup in index.html — no fetch needed since we only ever route
// to a single mainchat (id=default).

export async function loadSidebar() {
  try {
    // Always fetch include_archived=1 so the count and per-cwd disclosure
    // can show how many are archived even when collapsed. Payload size is
    // trivial at this scale.
    const res = await fetch('/api/sessions?include_archived=1&include_subagents=1');
    const sessions = res.ok ? (await res.json() || []) : [];
    // Cache the FULL list (incl. subagents): markViewing() re-renders the
    // sidebar from this cache on selection, and it must still see subagents
    // or expanded child rows would blink out until the next poll. Unread
    // bookkeeping stays root-only — subagents are read-only and never run.
    setLastSessions(sessions);
    reconcileUnread(sessions.filter(s => !s.is_subagent));
    renderSidebarSessions(sessions);
    updateSidebarActive();
    updateTabBadge();
  } catch {/* server may be down briefly */}
}

export function renderSidebarSessions(allSessions) {
  const wrap = document.getElementById('sidebar-sessions');
  const count = document.getElementById('sidebar-session-count');
  const roots = allSessions.filter(s => !s.is_subagent);
  const children = new Map();
  for (const s of allSessions) {
    if (!s.is_subagent || !s.parent_id) continue;
    if (!children.has(s.parent_id)) children.set(s.parent_id, []);
    children.get(s.parent_id).push(s);
  }
  // Keep the parent expanded while its subagent is the one on screen, so the
  // active child row stays visible across the 5s re-render.
  const active = allSessions.find(s => s.id === currentDetailId);
  if (active?.is_subagent && active.parent_id) subagentExpanded.add(active.parent_id);
  const visible = roots.filter(s => !s.archived);
  if (count) {
    count.textContent = visible.length === roots.length
      ? '(' + roots.length + ')'
      : '(' + visible.length + '/' + roots.length + ')';
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
  for (const s of roots) {
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
  // Order by last user input, not file mtime, so a session doesn't jump on
  // streaming or on pause/kill. Falls back to last_event_at when unset.
  const recencyKey = s => Date.parse(s.last_input_at) || Date.parse(s.last_event_at) || 0;
  const recencyOf = arr => Math.max(...arr.map(recencyKey));
  const byRecent = (a, b) => recencyKey(b) - recencyKey(a);
  // Sort cwd groups by their most-recent visible activity, not absolute
  // — stale cwds with one expanded archived row shouldn't jump to the top.
  cwds.sort((a, b) => {
    const av = groups.get(a).filter(s => !s.archived);
    const bv = groups.get(b).filter(s => !s.archived);
    return recencyOf(bv) - recencyOf(av);
  });

  // A single row. Subagent rows (child=true) are read-only: indented link,
  // no kebab. A root row carries the kebab, which offers "Show subagents"
  // when the session has any — the only way to reveal its children.
  const renderRow = (s, child = false) => {
    const href = '#/s/' + encodeURIComponent(s.id);
    const permissions = pendingPermissionCount(s.id);
    const dot = permissions
      ? `<span class="running-dot permission" title="${permissions} permission request${permissions === 1 ? '' : 's'} pending">●</span>`
      : isUnread(s)
        ? '<span class="running-dot unread" title="new response">●</span>'
        : statusDot(s.status);
    const auto = s.auto_approve
      ? '<span class="auto-dot" title="auto-approve enabled">ϟ</span>'
      : '';
    const title = s.title || '(untitled)';
    const mark = backendMark(s.backend);
    if (child) {
      return `<li class="sidebar-item subagent-row">
        <a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${mark}${dot}${auto}${esc(title)}</a>
      </li>`;
    }
    const liClass = s.archived ? 'sidebar-item archived-row' : 'sidebar-item';
    const subs = children.get(s.id) || [];
    const archAction = s.archived ? 'unarchive' : 'archive';
    const archTitle = s.archived ? 'unarchive' : 'archive';
    return `<li class="${liClass}">
      <a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${mark}${dot}${auto}${esc(title)}</a>
      <span class="row-actions">
        <button class="row-action-btn" type="button" data-quick="${archAction}" data-id="${esc(s.id)}" title="${archTitle}" aria-label="${archTitle}">
          ${s.archived
            ? '<svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2 5l6-3 6 3v6l-6 3-6-3V5z"/><path d="M2 5l6 3 6-3M8 8v6" transform="rotate(180 8 8)"/></svg>'
            : '<svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2 5l6-3 6 3v6l-6 3-6-3V5z"/><path d="M2 5l6 3 6-3M8 8v6"/></svg>'}
        </button>
        <button class="row-action-btn row-action-danger" type="button" data-quick="delete" data-id="${esc(s.id)}" title="delete" aria-label="delete">
          <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2 4h12M6 4V2.5A.5.5 0 0 1 6.5 2h3a.5.5 0 0 1 .5.5V4M3.5 4l.7 9a1 1 0 0 0 1 .9h5.6a1 1 0 0 0 1-.9l.7-9"/></svg>
        </button>
      </span>
      <button class="kebab-btn" type="button"
        data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}"
        data-pinned="${s.pinned ? '1' : '0'}"
        data-status="${esc(s.status || '')}"
        data-subagents="${subs.length}"
        aria-label="session actions" title="more">⋮</button>
    </li>`;
  };

  // A root row, plus its subagent rows when the user has toggled them on.
  const renderItem = s => {
    let html = renderRow(s);
    if (subagentExpanded.has(s.id)) {
      const subs = (children.get(s.id) || []).slice().sort(byRecent);
      html += subs.map(sub => renderRow(sub, true)).join('');
    }
    return html;
  };

  // Pinned sessions: fixed group above the scroll container.
  const pinnedEl = document.getElementById('sidebar-pinned');
  const pinnedItems = roots.filter(s => s.pinned && !s.archived).sort(byRecent);
  const pinnedHtml = pinnedItems.length
    ? `<div class="cwd-label"><span class="cwd-label-text pinned-label">Pinned</span></div>
       <ul class="sidebar-list">${pinnedItems.map(renderItem).join('')}</ul>` : '';
  if (pinnedEl && pinnedHtml !== lastPinnedHtml) {
    lastPinnedHtml = pinnedHtml;
    pinnedEl.innerHTML = pinnedHtml;
  }

  const html = cwds.map(cwd => {
    const all = groups.get(cwd);
    const visibleItems = all.filter(s => !s.archived && !s.pinned).sort(byRecent);
    const archivedItems = all.filter(s => s.archived).sort(byRecent);
    const expanded = cwdExpanded.has(cwd);
    if (!visibleItems.length && !archivedItems.length) return '';
    let lis = visibleItems.map(renderItem).join('');
    if (expanded) lis += archivedItems.map(renderItem).join('');
    const toggleRow = archivedItems.length === 0
      ? ''
      : `<button class="cwd-toggle-archived" type="button" data-cwd="${esc(cwd)}">${
          expanded ? '└ [ collapse ]' : '└ [ ' + archivedItems.length + ' archived ]'
        }</button>`;
    const newHere = cwd === '(unknown)'
      ? ''
      : `<a class="sidebar-new cwd-new" href="${esc('#/new/' + encodeURIComponent(cwd))}"
           title="new session here" aria-label="new session here">+</a>`;
    return `<div class="cwd-group">
      <div class="cwd-label">
        <span class="cwd-label-text" title="${esc(cwd)}">${esc(cwd)}</span>
        ${newHere}
      </div>
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

export function updateSidebarActive() {
  const hash = location.hash || '#/';
  document.querySelectorAll('.sidebar-newsession').forEach(a => {
    a.classList.toggle('active', hash === '#/new' || hash.startsWith('#/new/'));
  });
  let sessionKey = '';
  if (hash.startsWith('#/s/')) {
    sessionKey = 's:' + decodeURIComponent(hash.slice(4));
  }
  document.querySelectorAll('#sidebar a[data-route]').forEach(a => {
    a.classList.toggle('active', a.dataset.route === sessionKey);
  });
}

// Register renderSidebarSessions with state.js so markViewing can call it
// without a circular import.
registerRenderSidebarSessions(renderSidebarSessions);

// Sidebar toggle. On mobile (<720px) the sidebar is a fixed slide-in drawer
// toggled by the hamburger button. On desktop, a collapse button hides the
// sidebar entirely; the hamburger then re-appears to restore it.
const mobileToggle = document.getElementById('mobile-toggle');
const sidebarEl = document.getElementById('sidebar');
const sidebarCollapse = document.getElementById('sidebar-collapse');
if (mobileToggle && sidebarEl) {
  mobileToggle.addEventListener('click', () => {
    if (document.body.classList.contains('sidebar-collapsed')) {
      document.body.classList.remove('sidebar-collapsed');
    } else {
      sidebarEl.classList.toggle('open');
    }
  });
  window.addEventListener('hashchange', () => sidebarEl.classList.remove('open'));
}
if (sidebarCollapse) {
  sidebarCollapse.addEventListener('click', () => {
    if (sidebarEl && sidebarEl.classList.contains('open')) {
      sidebarEl.classList.remove('open');
    } else {
      document.body.classList.add('sidebar-collapsed');
    }
  });
}
const sidebarBackdrop = document.getElementById('sidebar-backdrop');
if (sidebarBackdrop && sidebarEl) {
  sidebarBackdrop.addEventListener('click', () => sidebarEl.classList.remove('open'));
}

// Sidebar width drag. The resizer is a 6px hit strip on the sidebar's right
// edge; dragging updates --sidebar-width live and persists to localStorage.
// Collapsing ignores the stored width until the sidebar is re-opened.
const resizer = document.getElementById('sidebar-resizer');
const SIDEBAR_W_KEY = 'usher.sidebarWidth';
const SIDEBAR_MIN = 180, SIDEBAR_MAX = 480;
function applySidebarWidth(px) {
  const w = Math.min(SIDEBAR_MAX, Math.max(SIDEBAR_MIN, px));
  document.documentElement.style.setProperty('--sidebar-width', w + 'px');
  return w;
}
try {
  const saved = parseInt(localStorage.getItem(SIDEBAR_W_KEY) || '', 10);
  if (saved) applySidebarWidth(saved);
} catch {/* private mode */}
if (resizer) {
  resizer.addEventListener('pointerdown', (e) => {
    if (window.innerWidth <= 720) return; // drawer mode: fixed width
    e.preventDefault();
    resizer.setPointerCapture(e.pointerId);
    document.body.classList.add('sidebar-resizing');
    const startX = e.clientX;
    const startW = sidebarEl.getBoundingClientRect().width;
    const onMove = (ev) => {
      applySidebarWidth(startW + (ev.clientX - startX));
    };
    const onUp = (ev) => {
      resizer.removeEventListener('pointermove', onMove);
      resizer.removeEventListener('pointerup', onUp);
      resizer.removeEventListener('pointercancel', onUp);
      document.body.classList.remove('sidebar-resizing');
      try {
        localStorage.setItem(SIDEBAR_W_KEY, String(parseInt(sidebarEl.getBoundingClientRect().width, 10)));
      } catch {/* private mode */}
    };
    resizer.addEventListener('pointermove', onMove);
    resizer.addEventListener('pointerup', onUp);
    resizer.addEventListener('pointercancel', onUp);
  });
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
  if (kebabOpenBtn) {
    kebabOpenBtn.classList.remove('open');
    kebabOpenBtn.closest('.sidebar-item')?.classList.remove('kebab-active');
  }
  kebabOpenBtn = null;
  kebabOpenFor = null;
}

function openKebabPopover(btn) {
  if (!kebabPopover) return;
  // Switching kebab-to-kebab skips closeKebabPopover, so clear the old row here.
  closeKebabPopover();
  const id = btn.dataset.id;
  const archived = btn.dataset.archived === '1';
  const action = archived ? 'unarchive' : 'archive';
  const label = archived ? 'Unarchive' : 'Archive';
  const pinned = btn.dataset.pinned === '1';
  const pinAction = pinned ? 'unpin' : 'pin';
  const pinLabel = pinned ? 'Unpin' : 'Pin';
  // Pause only applies to a session with a live window; an idle one has
  // nothing to tear down, so we hide it rather than offer a no-op.
  const status = btn.dataset.status;
  const pauseItem = (status === 'live' || status === 'running' || status === 'awaiting_permission')
    ? `<button type="button" class="kebab-item" data-action="pause" data-id="${esc(id)}">Pause</button>`
    : '';
  // Editor deep link — only when --editor-url is configured AND the button
  // carries a cwd (the title menu does; sidebar rows don't need it). A real
  // <a> so target="usher-editor" reuses one named tab across clicks — which
  // is also why there's no rel="noopener": it would sever the browsing
  // context group and name lookup with it, opening a fresh tab every click.
  // {cwd} is substituted verbatim: templates place it in a path
  // (vscode://file{cwd}) or query — encoding is the template author's call.
  const cwd = btn.dataset.cwd;
  const editorItem = (editorUrl && cwd)
    ? `<a class="kebab-item" data-action="editor" target="usher-editor"
         href="${esc(editorUrl.split('{cwd}').join(cwd))}">Open in editor</a>`
    : '';
  // Subagents are hidden by default; the kebab is the per-session opt-in to
  // reveal (or re-hide) this session's read-only child transcripts.
  const subCount = parseInt(btn.dataset.subagents || '0', 10);
  const subItem = subCount > 0
    ? `<button type="button" class="kebab-item" data-action="subagents" data-id="${esc(id)}">${
        subagentExpanded.has(id) ? 'Hide' : 'Show'} subagents</button>`
    : '';
  // Let standalone PWAs reload from the detail menu.
  const reloadItem = btn.classList.contains('subtitle-menu')
    ? `<button type="button" class="kebab-item" data-action="reload" data-id="${esc(id)}">Reload</button>`
    : '';
  kebabPopover.innerHTML =
    reloadItem +
    editorItem +
    subItem +
    `<button type="button" class="kebab-item" data-action="rename" data-id="${esc(id)}">Rename</button>` +
    `<button type="button" class="kebab-item" data-action="${pinAction}" data-id="${esc(id)}">${pinLabel}</button>` +
    `<button type="button" class="kebab-item" data-action="${action}" data-id="${esc(id)}">${label}</button>` +
    pauseItem +
    `<button type="button" class="kebab-item kebab-danger" data-action="delete" data-id="${esc(id)}">Delete</button>`;
  kebabPopover.hidden = false;
  // Position below the button — right-aligned for the edge-hugging kebabs,
  // left-aligned for the title menu (its anchor sits at the header's left).
  // Clamp to viewport edges so the menu stays fully visible on narrow screens.
  const r = btn.getBoundingClientRect();
  const popW = kebabPopover.offsetWidth;
  const popH = kebabPopover.offsetHeight;
  let left = btn.classList.contains('subtitle-menu') ? r.left : r.right - popW;
  if (left + popW > window.innerWidth - 4) left = window.innerWidth - popW - 4;
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
  // Highlight the owning row — on touch the kebab itself stays invisible.
  btn.closest('.sidebar-item')?.classList.add('kebab-active');
  kebabOpenBtn = btn;
}

document.addEventListener('click', (e) => {
  const quick = e.target.closest('.row-action-btn');
  if (quick) {
    e.preventDefault();
    e.stopPropagation();
    handleKebabAction(quick.dataset.quick, quick.dataset.id);
    return;
  }
  const cwdToggle = e.target.closest('.cwd-toggle-archived');
  if (cwdToggle) {
    e.preventDefault();
    const cwd = cwdToggle.dataset.cwd;
    if (cwdExpanded.has(cwd)) cwdExpanded.delete(cwd);
    else cwdExpanded.add(cwd);
    loadSidebar();
    return;
  }
  // .subtitle-menu (the detail header's title-as-menu button) shares the
  // popover and every action handler with the sidebar/list kebabs.
  const kebab = e.target.closest('.kebab-btn, .subtitle-menu');
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
    if (item.dataset.action === 'editor') {
      // Let the <a> navigate natively (named-tab target); just tidy up.
      closeKebabPopover();
      return;
    }
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

// ---------- Mobile long-press → actions menu ----------
//
// Touch never reveals the hover-gated kebab (see style.css), so a long-press
// on the row stands in for it: hold ~480ms to open the same popover, anchored
// to the row's invisible kebab button.
const LONG_PRESS_MS = 480;
const MOVE_CANCEL_PX = 10;
let lpTimer = null;
let lpKebab = null;
let lpStartX = 0, lpStartY = 0;
let recentTouch = false;   // distinguishes touch long-press from desktop right-click
let suppressNextClick = false; // swallow the click the finger-lift synthesises

function cancelLongPress() {
  if (lpTimer) { clearTimeout(lpTimer); lpTimer = null; }
  lpKebab = null;
}

if (sidebarEl) {
  sidebarEl.addEventListener('touchstart', (e) => {
    if (e.touches.length !== 1) return;
    const li = e.target.closest('.sidebar-item');
    const kebab = li && li.querySelector('.kebab-btn');
    if (!kebab) return;
    recentTouch = true;
    lpKebab = kebab;
    lpStartX = e.touches[0].clientX;
    lpStartY = e.touches[0].clientY;
    lpTimer = setTimeout(() => {
      lpTimer = null;
      if (!lpKebab) return;
      suppressNextClick = true;
      openKebabPopover(lpKebab);
      lpKebab = null;
      if (navigator.vibrate) navigator.vibrate(10);
    }, LONG_PRESS_MS);
  }, { passive: true });

  // A scroll/drag past the threshold means the user is panning, not pressing.
  sidebarEl.addEventListener('touchmove', (e) => {
    if (!lpTimer) return;
    const t = e.touches[0];
    if (Math.abs(t.clientX - lpStartX) > MOVE_CANCEL_PX ||
        Math.abs(t.clientY - lpStartY) > MOVE_CANCEL_PX) {
      cancelLongPress();
    }
  }, { passive: true });

  sidebarEl.addEventListener('touchend', () => {
    cancelLongPress();
    setTimeout(() => { recentTouch = false; }, 800);
  }, { passive: true });
  sidebarEl.addEventListener('touchcancel', () => {
    cancelLongPress();
    setTimeout(() => { recentTouch = false; }, 800);
  }, { passive: true });

  // Kill the native menu (Android long-press, desktop right-click). Desktop has
  // no touch path so right-click also opens the popover; Android's timer already did.
  sidebarEl.addEventListener('contextmenu', (e) => {
    const li = e.target.closest('.sidebar-item');
    if (!li) return;
    e.preventDefault();
    const kebab = li.querySelector('.kebab-btn');
    // Read-only subagent rows intentionally have no actions. Suppress the
    // browser's native link/text menu without substituting an empty popover.
    if (!kebab) return;
    if (recentTouch) return;
    openKebabPopover(kebab);
  });
}

// Lifting the finger after a long-press synthesises a click on the <a>; swallow
// that one click (capture phase, before it navigates) so the row doesn't open too.
document.addEventListener('click', (e) => {
  if (!suppressNextClick) return;
  suppressNextClick = false;
  if (e.target.closest('.sidebar-item')) {
    e.preventDefault();
    e.stopPropagation();
  }
}, true);

const settingsBtn = document.getElementById('settings-btn');
const settingsMenu = document.getElementById('settings-menu');
if (settingsBtn && settingsMenu) {
  settingsBtn.addEventListener('click', () => {
    settingsMenu.hidden = !settingsMenu.hidden;
  });
  document.addEventListener('click', (e) => {
    if (!settingsMenu.hidden && !e.target.closest('.sidebar-footer')) {
      settingsMenu.hidden = true;
    }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !settingsMenu.hidden) settingsMenu.hidden = true;
  });
}

async function handleKebabAction(action, id) {
  closeKebabPopover();
  if (action === 'reload') {
    location.reload();
    return;
  }
  if (action === 'subagents') {
    if (subagentExpanded.has(id)) subagentExpanded.delete(id);
    else subagentExpanded.add(id);
    loadSidebar();
    return;
  }
  if (action === 'rename') {
    renameSession(id);
    return;
  }
  if (action === 'delete') {
    deleteSession(id);
    return;
  }
  if (action === 'pause') {
    pauseSession(id);
    return;
  }
  if (action === 'pin' || action === 'unpin') {
    const method = action === 'pin' ? 'POST' : 'DELETE';
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/pin', { method });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      loadSidebar();
      loadList();
      if (id === currentDetailId) refreshSubtitle(id); // re-sync the header kebab datasets
    } catch (e) {
      console.warn('pin/unpin failed', e);
    }
    return;
  }
  const method = action === 'archive' ? 'POST' : 'DELETE';
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/archive', { method });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSidebar();
    loadList(); // no-op unless the list view is open
    if (id === currentDetailId) refreshSubtitle(id); // re-sync the header kebab datasets
  } catch (e) {
    console.warn('archive/unarchive failed', e);
  }
}

// ---------- Lightweight app modal (replaces native confirm/prompt) ----------

// appModal shows a small dialog and resolves with the action name clicked.
// actions: [{name, label, kind: 'primary'|'danger'|'plain'}]. Esc/overlay
// click resolves null. An input field is included when opts.input is set:
// {label, value, placeholder} — the resolve value is then {action, text}.
function appModal(opts) {
  return new Promise((resolve) => {
    const wrap = document.createElement('div');
    wrap.id = 'app-modal'; // NOT "modal" — interaction.js owns that id and
    // removes it on every poll when no questions are pending.
    const inputHtml = opts.input
      ? `<label class="app-modal-label">${esc(opts.input.label || '')}</label>` +
        `<input class="app-modal-input" type="text" value="${esc(opts.input.value || '')}" placeholder="${esc(opts.input.placeholder || '')}">`
      : '';
    const buttons = opts.actions.map(a =>
      `<button type="button" class="app-modal-btn app-modal-${a.kind || 'plain'}" data-action="${esc(a.name)}">${esc(a.label)}</button>`
    ).join('');
    wrap.innerHTML = `
      <div class="overlay"></div>
      <div class="dialog app-modal">
        <h3>${esc(opts.title || '')}</h3>
        ${opts.body ? `<div class="app-modal-body">${esc(opts.body)}</div>` : ''}
        ${inputHtml}
        <div class="app-modal-actions">${buttons}</div>
      </div>`;
    document.body.appendChild(wrap);
    const input = wrap.querySelector('.app-modal-input');
    if (input) { input.focus(); input.select(); }
    const done = (action) => {
      wrap.remove();
      document.removeEventListener('keydown', onKey, true);
      if (action && opts.input) resolve({ action, text: input.value });
      else resolve(action ? { action } : null);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') { e.stopPropagation(); done(null); }
      if (e.key === 'Enter' && opts.input) { e.stopPropagation(); done(opts.actions[0].name); }
    };
    document.addEventListener('keydown', onKey, true);
    wrap.querySelector('.overlay').addEventListener('click', () => done(null));
    wrap.querySelectorAll('.app-modal-btn').forEach(b =>
      b.addEventListener('click', () => done(b.dataset.action)));
  });
}

// deleteSession permanently removes a session (and its transcript) after a
// confirm — irreversible, unlike archive. If the deleted session is the one on
// screen, route home so the detail view doesn't sit on a now-404 id.
async function deleteSession(id) {
  const res = await appModal({
    title: 'Delete session?',
    body: 'The conversation transcript will be removed permanently and cannot be recovered.',
    actions: [
      { name: 'delete', label: 'Delete', kind: 'danger' },
      { name: 'cancel', label: 'Cancel', kind: 'plain' },
    ],
  });
  if (!res || res.action !== 'delete') return;
  try {
    const res2 = await fetch('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
    if (!res2.ok) throw new Error('HTTP ' + res2.status);
    if (location.hash === '#/s/' + encodeURIComponent(id)) {
      location.hash = '#/';
    }
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('delete failed', e);
    appModal({ title: 'Delete failed', body: String(e.message || e), actions: [{ name: 'ok', label: 'OK', kind: 'primary' }] });
  }
}

// pauseSession tears down the session's live backend worker without deleting
// anything — the conversation stays on disk and resumes on the next send.
// Non-destructive, so no confirm. The session simply drops back to "idle".
async function pauseSession(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/pause', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSidebar();
    loadList(); // no-op unless the list view is open
    if (id === currentDetailId) refreshSubtitle(id); // re-sync the header kebab datasets
  } catch (e) {
    console.warn('pause failed', e);
    alert('Failed to pause session.');
  }
}

async function renameSession(id) {
  const res = await appModal({
    title: 'Rename session',
    input: { label: 'title (empty resets to the derived title)', value: '', placeholder: 'session title' },
    actions: [
      { name: 'save', label: 'Save', kind: 'primary' },
      { name: 'cancel', label: 'Cancel', kind: 'plain' },
    ],
  });
  if (!res || res.action !== 'save') return;
  try {
    const res2 = await fetch('/api/sessions/' + encodeURIComponent(id) + '/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: res.text || '' }),
    });
    if (!res2.ok) throw new Error('HTTP ' + res2.status);
    loadSidebar();
    loadList();
    if (id === currentDetailId) refreshSubtitle(id);
  } catch (e) {
    console.warn('rename failed', e);
  }
}
