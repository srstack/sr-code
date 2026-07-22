// usher SPA: list view.

import {
  esc, fmt, root, subtitle, closeES,
  setCurrentDetailId, setCurrentDraftKey,
  listInterval, clearListInterval, setListInterval,
} from './state.js';
import { statusDot, backendMark } from './render.js';
import { appModal } from './sidebar.js';
import { loadSidebar } from './sidebar.js';

// --- list-private state ---
let lastListRowsHtml = '';
let lastCwdSig = ''; // distinct-cwd set the list's cwd <select> was built from
const selected = new Set(); // ids checked for batch actions; survives polls

// ---------- List view ----------

export async function showList() {
  closeES();
  setCurrentDetailId(null);
  setCurrentDraftKey(null);
  selected.clear();
  subtitle.textContent = 'discovered sessions';
  // Stable shell: pinned controls + a .list-scroll wrapper (the scroll
  // container — <main> is overflow:hidden). loadList only swaps the rows, so
  // the 5s poll doesn't disturb the controls.
  root.innerHTML = `
    <div class="list-controls">
      <select id="list-cwd"><option value="">all folders</option></select>
      <label class="archived-toggle"><input type="checkbox" id="list-archived"> show archived</label>
      <span id="list-selection" class="list-selection" hidden>
        <span id="list-sel-count"></span>
        <button id="list-sel-delete" class="list-sel-delete" type="button">Delete</button>
        <button id="list-sel-clear" class="list-sel-clear" type="button">Clear</button>
      </span>
    </div>
    <div class="list-scroll">
      <table>
        <colgroup><col class="col-sel"><col><col class="col-cwd"><col class="col-when"><col class="col-act"></colgroup>
        <thead><tr><th class="sel"><input type="checkbox" id="list-check-all" title="select all shown"></th><th>title</th><th>cwd</th><th>last activity</th><th aria-label="actions"></th></tr></thead>
        <tbody id="list-rows"></tbody>
      </table>
    </div>`;
  lastListRowsHtml = '';
  lastCwdSig = '';
  const cEl = document.getElementById('list-cwd');
  if (cEl) cEl.addEventListener('change', applyListFilter);
  const aEl = document.getElementById('list-archived');
  if (aEl) aEl.addEventListener('change', applyListFilter);
  const allEl = document.getElementById('list-check-all');
  if (allEl) allEl.addEventListener('change', toggleSelectAll);
  const delEl = document.getElementById('list-sel-delete');
  if (delEl) delEl.addEventListener('click', deleteSelected);
  const clrEl = document.getElementById('list-sel-clear');
  if (clrEl) clrEl.addEventListener('click', () => { selected.clear(); syncCheckboxes(); });
  if (!listInterval) setListInterval(setInterval(loadList, 5000));
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
  syncSelectAllState();
}

// ---------- batch selection ----------

function visibleIds() {
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return [];
  return [...rowsEl.querySelectorAll('tr[data-id]')]
    .filter(tr => tr.style.display !== 'none')
    .map(tr => tr.dataset.id);
}

function toggleSelectAll() {
  const allEl = document.getElementById('list-check-all');
  const ids = visibleIds();
  if (allEl && allEl.checked) ids.forEach(id => selected.add(id));
  else ids.forEach(id => selected.delete(id));
  syncCheckboxes();
}

// syncCheckboxes repaints checkbox states from the Set and updates the bar.
function syncCheckboxes() {
  const rowsEl = document.getElementById('list-rows');
  if (rowsEl) {
    rowsEl.querySelectorAll('.row-check').forEach(cb => {
      cb.checked = selected.has(cb.dataset.id);
    });
  }
  syncSelectAllState();
  const bar = document.getElementById('list-selection');
  const count = document.getElementById('list-sel-count');
  if (bar && count) {
    bar.hidden = selected.size === 0;
    count.textContent = selected.size + ' selected';
  }
}

function syncSelectAllState() {
  const allEl = document.getElementById('list-check-all');
  if (!allEl) return;
  const ids = visibleIds();
  const n = ids.filter(id => selected.has(id)).length;
  allEl.checked = ids.length > 0 && n === ids.length;
  allEl.indeterminate = n > 0 && n < ids.length;
}

async function deleteSelected() {
  if (!selected.size) return;
  const n = selected.size;
  const res = await appModal({
    title: `Delete ${n} session${n === 1 ? '' : 's'}?`,
    body: 'The conversation transcripts will be removed permanently and cannot be recovered.',
    actions: [
      { name: 'delete', label: `Delete ${n}`, kind: 'danger' },
      { name: 'cancel', label: 'Cancel', kind: 'plain' },
    ],
  });
  if (!res || res.action !== 'delete') return;
  const ids = [...selected];
  selected.clear();
  syncCheckboxes();
  let failed = 0;
  for (const id of ids) {
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(id), { method: 'DELETE' });
      if (!res.ok) failed++;
    } catch { failed++; }
  }
  loadList();
  loadSidebar();
  if (failed) {
    appModal({
      title: 'Some deletions failed',
      body: `${failed} of ${ids.length} sessions could not be deleted.`,
      actions: [{ name: 'ok', label: 'OK', kind: 'primary' }],
    });
  }
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

export async function loadList() {
  if (location.hash && location.hash !== '#/' && location.hash !== '') return;
  const rowsEl = document.getElementById('list-rows');
  if (!rowsEl) return; // shell not built (not on the list view)
  try {
    // Always fetch the full set (incl. archived); the controls narrow it
    // client-side, so toggling "show archived" needs no refetch.
    const res = await fetch('/api/sessions?include_archived=1');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = (await res.json()) || [];
    const sorted = data.slice().sort((a, b) =>
      (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0));
    const html = sorted.length ? sorted.map(s => {
      const title = s.title || '(untitled)';
      const dot = statusDot(s.status);
      const checked = selected.has(s.id) ? ' checked' : '';
      return `
      <tr data-id="${esc(s.id)}" data-cwd="${esc(s.cwd || '')}" data-archived="${s.archived ? '1' : ''}" class="${s.archived ? 'archived' : ''}">
        <td class="sel"><input type="checkbox" class="row-check" data-id="${esc(s.id)}"${checked}></td>
        <td class="title" title="${esc(title)}">${backendMark(s.backend)}${dot ? dot + ' ' : ''}${esc(title)}</td>
        <td class="cwd" title="${esc(s.cwd || '')}">${esc(s.cwd || '')}</td>
        <td>${esc(fmt(s.last_event_at))}</td>
        <td class="act"><button class="kebab-btn" type="button" data-id="${esc(s.id)}" data-archived="${s.archived ? '1' : '0'}" data-pinned="${s.pinned ? '1' : '0'}" data-status="${esc(s.status || '')}" aria-label="session actions" title="more">⋮</button></td>
      </tr>`;
    }).join('') : '<tr><td colspan="5" class="muted" style="padding:0.75rem">no sessions found</td></tr>';
    // Skip the rebuild when unchanged so status-dot animations don't restart
    // and the current filter view is left untouched.
    if (html !== lastListRowsHtml) {
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
      rowsEl.querySelectorAll('.row-check').forEach(cb => {
        cb.addEventListener('click', (e) => {
          e.stopPropagation();
          if (cb.checked) selected.add(cb.dataset.id);
          else selected.delete(cb.dataset.id);
          syncCheckboxes();
        });
      });
    }
    updateCwdOptions([...new Set(data.map(s => s.cwd).filter(Boolean))].sort());
    applyListFilter(); // keep the active filters applied across polls
    syncCheckboxes();
  } catch (e) {
    rowsEl.innerHTML = '<tr><td colspan="5" class="err" style="padding:0.75rem">failed to load: ' + esc(String(e)) + '</td></tr>';
    lastListRowsHtml = '';
  }
}
