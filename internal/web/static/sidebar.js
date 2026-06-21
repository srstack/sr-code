// usher SPA: sidebar rendering, kebab popover, toggle, settings menu.

import {
  esc, cwdExpanded,
  isUnread, reconcileUnread,
  setLastSessions,
  updateTabBadge,
  registerRenderSidebarSessions,
} from './state.js';
import { statusDot, backendMark } from './render.js';
import { loadList } from './list.js';

// --- sidebar-private state ---
// Last sidebar HTML written to the DOM. The sidebar re-polls every 5s; skipping
// the innerHTML rewrite when nothing changed keeps the live-dot CSS animation
// from restarting (jumping back to its bright peak) on every poll.
let lastSidebarHtml = '';

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
    const res = await fetch('/api/sessions?include_archived=1');
    const sessions = res.ok ? (await res.json() || []) : [];
    setLastSessions(sessions);
    reconcileUnread(sessions);
    renderSidebarSessions(sessions);
    updateSidebarActive();
    updateTabBadge();
  } catch {/* server may be down briefly */}
}

export function renderSidebarSessions(allSessions) {
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

  const renderItem = s => {
    const href = '#/s/' + encodeURIComponent(s.id);
    const dot = isUnread(s)
      ? '<span class="running-dot unread" title="new response">●</span>'
      : statusDot(s.status);
    const auto = s.auto_approve
      ? '<span class="auto-dot" title="auto-approve enabled">ϟ</span>'
      : '';
    const title = s.title || '(untitled)';
    const liClass = s.archived ? 'sidebar-item archived-row' : 'sidebar-item';
    return `<li class="${liClass}">
      <a href="${esc(href)}" data-route="s:${esc(s.id)}" title="${esc(title)}">${dot}${auto}${esc(title)}</a>
      <button class="kebab-btn" type="button"
        data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}"
        data-status="${esc(s.status || '')}"
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
  const inMainChat = hash === '#/chat' || hash.startsWith('#/chat/');
  document.querySelectorAll('.sidebar-mainchat').forEach(a => {
    a.classList.toggle('active', inMainChat);
  });
  document.querySelectorAll('.sidebar-new:not(.cwd-new)').forEach(a => {
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
  const label = archived ? 'Unarchive' : 'Archive';
  // Pause only applies to a session with a live window; an idle one has
  // nothing to tear down, so we hide it rather than offer a no-op.
  const status = btn.dataset.status;
  const pauseItem = (status === 'live' || status === 'running' || status === 'awaiting_permission')
    ? `<button type="button" class="kebab-item" data-action="pause" data-id="${esc(id)}">Pause</button>`
    : '';
  kebabPopover.innerHTML =
    `<button type="button" class="kebab-item" data-action="${action}" data-id="${esc(id)}">${label}</button>` +
    pauseItem +
    `<button type="button" class="kebab-item kebab-danger" data-action="delete" data-id="${esc(id)}">Delete</button>`;
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
  if (action === 'delete') {
    deleteSession(id);
    return;
  }
  if (action === 'pause') {
    pauseSession(id);
    return;
  }
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

// deleteSession permanently removes a session (and its transcript) after a
// confirm — irreversible, unlike archive. If the deleted session is the one on
// screen, route home so the detail view doesn't sit on a now-404 id.
async function deleteSession(id) {
  if (!confirm('Delete this session permanently? Its conversation transcript will be removed and cannot be recovered.')) {
    return;
  }
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    if (location.hash === '#/s/' + encodeURIComponent(id)) {
      location.hash = '#/';
    }
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('delete failed', e);
    alert('Failed to delete session.');
  }
}

// pauseSession tears down the session's live TUI window without deleting
// anything — the conversation stays on disk and resumes on the next send.
// Non-destructive, so no confirm. The session simply drops back to "idle".
async function pauseSession(id) {
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(id) + '/pause', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSidebar();
    loadList(); // no-op unless the list view is open
  } catch (e) {
    console.warn('pause failed', e);
    alert('Failed to pause session.');
  }
}
