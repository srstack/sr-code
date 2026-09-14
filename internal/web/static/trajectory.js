// usher SPA: dsh-style compact trajectory view.
//
// A projection of the session detail page's ALREADY-LOADED transcript turns
// (detail.js sessionTurns + the in-flight live turn — no fetches here) into
// fixed-height rows: one per turn part (a tool call+result pair arrives as a
// single part; thinking parts fold into their assistant record), rendered as
// a windowed list when long. Per-turn collapsing groups everything under the
// user-turn header rows; a collapsed turn shows one 20px summary row.
//
// On top of the list sit a dsh-style minimap strip (three lanes: input =
// user/system, model = assistant/compaction, tools = tool; one span per
// record, turn-boundary ticks, click to scroll) and an AND-search box that
// dims non-matching rows and highlights matching spans.
//
// Consumed by detail.js (import) and by later work (global):
//   window.usherTrajectory = { project(turns) → records, render(container, turns, opts) }

import { esc } from './state.js';
import { formatDuration, formatTokens } from './render.js';

const ROW_H = 30;        // fixed row height (binding)
const SUMMARY_H = 20;    // collapsed turn summary row height
const WINDOW_MIN = 100;  // windowed rendering only beyond this many rows
const OVERSCAN = 12;     // extra rows rendered above/below the viewport
const TAIL_PX = 2;       // tail-follow threshold: only follow when at bottom
// Minimap DOM caps: beyond MAX_MAP_SPANS records the strip stride-samples
// (every ceil(n/MAX)-th record is drawn), so a 10k-record session renders
// ~2k spans, not 10k. Sampling is on the shared x-axis, so all three lanes
// keep their shape; click precision degrades to the nearest sampled span.
const MAX_MAP_SPANS = 2000;
const MAX_MAP_TICKS = 500;
const SEARCH_DEBOUNCE_MS = 300;

// project maps transcript turns to one record per visible row:
//   { id, kind: 'user'|'assistant'|'tool'|'compaction'|'system',
//     text, right, usage?, durationMs?, toolName?, toolTarget?, isError?,
//     turnIndex }
// turnIndex is the 1-based user-turn number the record belongs to (0 for
// anything before the first user turn — such records aren't collapsible).
export function project(turns) {
  const records = [];
  let turnIndex = 0;
  let seq = 0;
  const push = (rec) => { rec.id = 'r' + (seq++); records.push(rec); return rec; };
  for (const t of turns || []) {
    if (!t || typeof t !== 'object') continue;
    const role = t.role || '';
    if (role === 'user') {
      turnIndex++;
      push({ kind: 'user', text: oneLine(t.content), right: clock(t.ts), turnIndex });
      continue;
    }
    if (role === 'summary' || (role === 'system' && typeof t.content === 'string' && t.content.startsWith('Context compacted'))) {
      push({
        kind: 'compaction',
        text: role === 'summary' ? 'earlier conversation compressed' : oneLine(t.content),
        right: clock(t.ts),
        turnIndex,
      });
      continue;
    }
    if (role !== 'assistant') {
      // system / error / unknown roles: a single muted row.
      push({
        kind: 'system',
        text: oneLine(t.content),
        right: clock(t.ts),
        isError: role === 'error' || undefined,
        turnIndex,
      });
      continue;
    }
    const parts = Array.isArray(t.parts) ? t.parts : [];
    if (!parts.length) {
      if (t.content) {
        push({ kind: 'assistant', text: oneLine(t.content), right: clock(t.ts), usage: t.usage || undefined, turnIndex });
      }
      continue;
    }
    // Thinking folds into its assistant record, never a row of its own:
    // forward into the next text part's record when one follows in this
    // turn, otherwise back into the previous one. A thinking-only turn
    // still gets one assistant row (live turns pass through here).
    const textAfter = new Array(parts.length).fill(false);
    let seenText = false;
    for (let i = parts.length - 1; i >= 0; i--) {
      textAfter[i] = seenText;
      if (parts[i].type !== 'tool' && parts[i].type !== 'thinking') seenText = true;
    }
    let pendingMs = 0;  // forward-folding thinking span
    let danglingMs = 0; // thinking span no text part follows
    let sawThinking = false;
    let lastAssistant = null;
    let usagePending = t.usage || null;
    const takeUsage = () => { const u = usagePending; usagePending = null; return u || undefined; };
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i];
      if (p.type === 'thinking') {
        sawThinking = true;
        if (textAfter[i]) pendingMs += p.duration_ms || 0;
        else danglingMs += p.duration_ms || 0;
        continue;
      }
      if (p.type === 'tool') {
        // Call and result are already paired in this one part (an empty
        // content marks a still-running call) — one row carries both.
        push({
          kind: 'tool',
          text: (p.toolName || 'tool') + (p.toolTarget ? ' ' + p.toolTarget : ''),
          right: clock(p.ts || t.ts),
          durationMs: p.duration_ms || undefined,
          toolName: p.toolName || undefined,
          toolTarget: p.toolTarget || undefined,
          turnIndex,
        });
        continue;
      }
      lastAssistant = push({
        kind: 'assistant',
        text: oneLine(p.content),
        right: clock(p.ts || t.ts),
        usage: takeUsage(),
        durationMs: pendingMs || undefined,
        turnIndex,
      });
      pendingMs = 0;
    }
    if (danglingMs && lastAssistant) {
      lastAssistant.durationMs = (lastAssistant.durationMs || 0) + danglingMs;
    }
    if (!lastAssistant && sawThinking) {
      push({
        kind: 'assistant',
        text: '',
        right: clock(t.ts),
        usage: takeUsage(),
        durationMs: (pendingMs + danglingMs) || undefined,
        turnIndex,
      });
    }
  }
  return records;
}

// One mutable view at a time — the detail page is the only host, and it
// re-mounts (new container) on every navigation.
let view = null;

// render projects turns and paints the (windowed) rows into container.
// Tail-follow: when the list is scrolled to the bottom (2px threshold) it
// stays pinned as rows grow; a scrolled-up reader is never yanked.
export function render(container, turns, opts) {
  if (!view || view.root !== container) mount(container);
  const v = view;
  const list = v.list;
  // Capture stick-to-bottom intent BEFORE mutating the geometry.
  const stick = v.fresh || (list.scrollHeight - list.scrollTop - list.clientHeight <= TAIL_PX);
  v.fresh = false;
  v.records = project(turns);
  applySearch(v);
  v.rowsVersion++;
  buildRows(v);
  const y = stick ? Math.max(0, v.total - list.clientHeight) : list.scrollTop;
  paint(v, y);
  if (stick) list.scrollTop = list.scrollHeight;
  updateToolbar(v);
  scheduleMinimap(v);
}

function mount(container) {
  container.innerHTML =
    '<div class="traj-toolbar">' +
      '<button type="button" class="traj-collapse-all">collapse all</button>' +
      '<button type="button" class="traj-mode" title="minimap span widths: equal (sequence) or proportional to duration">sequence</button>' +
      '<input type="search" class="traj-search" placeholder="search — all terms must match" aria-label="search rows">' +
      '<span class="traj-count muted"></span>' +
    '</div>' +
    '<div class="traj-minimap">' +
      '<div class="traj-ticks"></div>' +
      '<div class="traj-lane traj-lane-input"></div>' +
      '<div class="traj-lane traj-lane-model"></div>' +
      '<div class="traj-lane traj-lane-tools"></div>' +
    '</div>' +
    '<div class="traj-list" tabindex="0"></div>';
  view = {
    root: container,
    list: container.querySelector('.traj-list'),
    countEl: container.querySelector('.traj-count'),
    collapseBtn: container.querySelector('.traj-collapse-all'),
    mapEl: container.querySelector('.traj-minimap'),
    ticksEl: container.querySelector('.traj-ticks'),
    laneEls: Array.from(container.querySelectorAll('.traj-lane')),
    modeBtn: container.querySelector('.traj-mode'),
    searchEl: container.querySelector('.traj-search'),
    collapsed: new Set(), // turnIndexes, survives re-renders while mounted
    groups: [],
    records: [],
    rows: [],
    total: 0,
    rowsVersion: 0,
    lastPaintKey: '',
    fresh: true, // first render pins to the tail (most recent activity)
    scrollRAF: 0,
    mapMode: 'sequence', // 'sequence' (equal widths) | 'duration' (∝ durationMs)
    mapRAF: 0,
    mapKey: '',
    mapSpans: [],        // rendered spans as {id, mid} for click-hit mapping
    terms: [],           // active search terms (lowercase, AND semantics)
    hits: null,          // Set of matching record ids, null when search off
    searchVersion: 0,
    searchTimer: 0,
  };
  const v = view;
  v.collapseBtn.addEventListener('click', () => toggleAll(v));
  v.modeBtn.addEventListener('click', () => {
    v.mapMode = v.mapMode === 'sequence' ? 'duration' : 'sequence';
    v.modeBtn.textContent = v.mapMode;
    scheduleMinimap(v);
  });
  v.searchEl.addEventListener('input', () => {
    clearTimeout(v.searchTimer);
    v.searchTimer = setTimeout(() => applyTerms(v), SEARCH_DEBOUNCE_MS);
  });
  v.searchEl.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && v.searchEl.value) {
      v.searchEl.value = '';
      clearTimeout(v.searchTimer);
      applyTerms(v);
    }
  });
  // Click a span to scroll to its record; click empty strip to scroll to
  // the record nearest the click's x position.
  v.mapEl.addEventListener('click', (e) => {
    const span = e.target.closest && e.target.closest('.traj-span');
    if (span) { scrollToRecord(v, span.dataset.id); return; }
    if (!v.mapSpans.length) return;
    const rect = v.mapEl.getBoundingClientRect();
    if (!rect.width) return;
    const fx = (e.clientX - rect.left) / rect.width * 100;
    let best = v.mapSpans[0], bd = Math.abs(best.mid - fx);
    for (const s of v.mapSpans) {
      const d = Math.abs(s.mid - fx);
      if (d < bd) { bd = d; best = s; }
    }
    scrollToRecord(v, best.id);
  });
  v.list.addEventListener('scroll', () => {
    if (v.rows.length <= WINDOW_MIN || v.scrollRAF) return;
    v.scrollRAF = requestAnimationFrame(() => {
      v.scrollRAF = 0;
      paint(v, v.list.scrollTop);
    });
  });
  // Click a turn header toggles its collapse; double-click anywhere in the
  // turn does the same (a dblclick on the header nets one toggle: its two
  // clicks cancel, the dblclick toggles).
  v.list.addEventListener('click', (e) => {
    const row = e.target.closest && e.target.closest('.traj-header');
    if (row) toggleTurn(v, Number(row.dataset.turn));
  });
  v.list.addEventListener('dblclick', (e) => {
    const row = e.target.closest && e.target.closest('.traj-row[data-turn]');
    if (row) toggleTurn(v, Number(row.dataset.turn));
  });
}

// buildRows flattens records into display rows, grouping under user-turn
// headers and substituting one summary row per collapsed turn. Row offsets
// are prefix-summed (rows have two fixed heights) for the windowing bisect.
function buildRows(v) {
  const groups = [];
  let cur = { header: null, members: [], steps: 0, tools: 0 };
  for (const rec of v.records) {
    if (rec.kind === 'user') {
      groups.push(cur);
      cur = { header: rec, members: [], steps: 0, tools: 0 };
      continue;
    }
    cur.members.push(rec);
    if (rec.kind === 'assistant') cur.steps++;
    else if (rec.kind === 'tool') cur.tools++;
  }
  groups.push(cur);
  v.groups = groups;
  const rows = [];
  for (const g of groups) {
    const collapsed = g.header && v.collapsed.has(g.header.turnIndex);
    if (g.header) rows.push({ rec: g.header, h: ROW_H, offset: 0 });
    if (collapsed) {
      if (g.members.length) {
        rows.push({ summary: true, turnIndex: g.header.turnIndex, steps: g.steps, tools: g.tools, h: SUMMARY_H, offset: 0 });
      }
    } else {
      for (const m of g.members) rows.push({ rec: m, h: ROW_H, offset: 0 });
    }
  }
  let off = 0;
  for (const r of rows) { r.offset = off; off += r.h; }
  v.rows = rows;
  v.total = off;
}

function toggleTurn(v, turnIndex) {
  if (!turnIndex) return;
  if (v.collapsed.has(turnIndex)) v.collapsed.delete(turnIndex);
  else v.collapsed.add(turnIndex);
  v.rowsVersion++;
  buildRows(v);
  paint(v, v.list.scrollTop);
  updateToolbar(v);
}

function toggleAll(v) {
  const collapsible = v.groups.filter(g => g.header && g.members.length);
  const allCollapsed = collapsible.length > 0 && collapsible.every(g => v.collapsed.has(g.header.turnIndex));
  if (allCollapsed) v.collapsed.clear();
  else for (const g of collapsible) v.collapsed.add(g.header.turnIndex);
  v.rowsVersion++;
  buildRows(v);
  paint(v, v.list.scrollTop);
  updateToolbar(v);
}

function updateToolbar(v) {
  v.countEl.textContent = v.hits
    ? v.hits.size + '/' + v.records.length + ' rows'
    : v.records.length + ' rows';
  const collapsible = v.groups.filter(g => g.header && g.members.length);
  const allCollapsed = collapsible.length > 0 && collapsible.every(g => v.collapsed.has(g.header.turnIndex));
  v.collapseBtn.textContent = allCollapsed ? 'expand all' : 'collapse all';
}

// applyTerms commits the search box value (300ms after the last keystroke):
// whitespace-split lowercase terms, AND semantics over record text.
function applyTerms(v) {
  v.terms = v.searchEl.value.toLowerCase().split(/\s+/).filter(Boolean);
  applySearch(v);
  v.rowsVersion++;
  paint(v, v.list.scrollTop);
  updateToolbar(v);
  scheduleMinimap(v);
}

// applySearch recomputes the hit set for the current terms. Non-matching
// rows are dimmed (never removed) via the hits set consulted in rowHTML.
function applySearch(v) {
  v.searchVersion++;
  if (!v.terms.length) { v.hits = null; return; }
  const hits = new Set();
  for (const rec of v.records) {
    const text = rec.text.toLowerCase();
    if (v.terms.every(t => text.includes(t))) hits.add(rec.id);
  }
  v.hits = hits;
}

// scrollToRecord smooth-scrolls the list to a record's row, expanding its
// turn first when collapsed (a collapsed turn has no row to land on). The
// existing scroll listener drives the windowed repaint as the list moves.
function scrollToRecord(v, id) {
  let idx = v.rows.findIndex(r => r.rec && r.rec.id === id);
  if (idx < 0) {
    const rec = v.records.find(r => r.id === id);
    if (!rec) return;
    if (rec.turnIndex && v.collapsed.has(rec.turnIndex)) {
      v.collapsed.delete(rec.turnIndex);
      v.rowsVersion++;
      buildRows(v);
      updateToolbar(v);
      idx = v.rows.findIndex(r => r.rec && r.rec.id === id);
    }
    if (idx < 0) return;
  }
  v.fresh = false;
  const y = Math.max(0, v.rows[idx].offset - ROW_H);
  v.list.scrollTo({ top: y, behavior: 'smooth' });
}

// scheduleMinimap coalesces minimap repaints with the same rAF pattern the
// list uses for scroll paints (render() itself is rAF-throttled by the
// caller on live updates).
function scheduleMinimap(v) {
  if (v.mapRAF) return;
  v.mapRAF = requestAnimationFrame(() => {
    v.mapRAF = 0;
    paintMinimap(v);
  });
}

// paintMinimap draws one span per record into its lane. Sequence mode gives
// every record the same width; duration mode scales widths ∝ durationMs and
// keeps the sequence width for records without a duration (timed records
// split the width that remains). Errors draw in the theme's danger color,
// search hits in the accent color, user records mark turn-boundary ticks.
// Spans carry no text: there is nothing to escape beyond the safe record id.
function paintMinimap(v) {
  const key = v.rowsVersion + ':' + v.mapMode + ':' + v.searchVersion;
  if (key === v.mapKey) return;
  v.mapKey = key;
  const recs = v.records;
  const n = recs.length;
  if (!n) {
    for (const el of v.laneEls) el.innerHTML = '';
    v.ticksEl.innerHTML = '';
    v.mapSpans = [];
    return;
  }
  const widths = new Array(n);
  const seqW = 100 / n;
  let durSum = 0, durCount = 0;
  if (v.mapMode === 'duration') {
    for (const r of recs) if (r.durationMs > 0) { durSum += r.durationMs; durCount++; }
  }
  const rem = 100 - (n - durCount) * seqW;
  if (!durCount || rem <= 0) {
    widths.fill(seqW);
  } else {
    const k = rem / durSum;
    for (let i = 0; i < n; i++) widths[i] = recs[i].durationMs > 0 ? recs[i].durationMs * k : seqW;
  }
  let users = 0;
  for (const r of recs) if (r.kind === 'user') users++;
  const tickStride = Math.max(1, Math.ceil(users / MAX_MAP_TICKS));
  const stride = Math.max(1, Math.ceil(n / MAX_MAP_SPANS));
  const lanes = ['', '', ''];
  const spans = [];
  let ticks = '', x = 0, ui = 0;
  for (let i = 0; i < n; i++) {
    const rec = recs[i];
    const left = x;
    x += widths[i];
    if (rec.kind === 'user' && ui++ % tickStride === 0) {
      ticks += '<i class="traj-tick" style="left:' + left.toFixed(4) + '%"></i>';
    }
    if (i % stride) continue;
    const lane = rec.kind === 'user' || rec.kind === 'system' ? 0 : rec.kind === 'tool' ? 2 : 1;
    let cls = 'traj-span';
    if (v.hits && v.hits.has(rec.id)) cls += ' traj-span-hit';
    if (rec.isError) cls += ' traj-span-err';
    lanes[lane] += '<span class="' + cls + '" data-id="' + esc(rec.id) + '" style="left:' +
      left.toFixed(4) + '%;width:' + widths[i].toFixed(4) + '%"></span>';
    spans.push({ id: rec.id, mid: left + widths[i] / 2 });
  }
  for (let l = 0; l < 3; l++) v.laneEls[l].innerHTML = lanes[l];
  v.ticksEl.innerHTML = ticks;
  v.mapSpans = spans;
}

// paint writes the visible window ± OVERSCAN with spacer divs holding the
// out-of-window height. At or below WINDOW_MIN rows everything renders
// directly. The innerHTML cache key skips no-op rewrites.
function paint(v, y) {
  const rows = v.rows;
  if (rows.length <= WINDOW_MIN) {
    const key = 'all:' + v.rowsVersion;
    if (v.lastPaintKey !== key) {
      v.lastPaintKey = key;
      v.list.innerHTML = rows.map(r => rowHTML(r, v.hits)).join('');
    }
    return;
  }
  const viewH = v.list.clientHeight || 600;
  const start = Math.max(0, firstVisible(rows, y) - OVERSCAN);
  let end = start;
  while (end < rows.length && rows[end].offset < y + viewH) end++;
  end = Math.min(rows.length, end + OVERSCAN);
  const topH = start < rows.length ? rows[start].offset : 0;
  const botH = v.total - (end < rows.length ? rows[end].offset : v.total);
  const key = 'win:' + v.rowsVersion + ':' + start + ':' + end;
  if (key === v.lastPaintKey) return;
  v.lastPaintKey = key;
  v.list.innerHTML =
    '<div class="traj-spacer" style="height:' + topH + 'px"></div>' +
    rows.slice(start, end).map(r => rowHTML(r, v.hits)).join('') +
    '<div class="traj-spacer" style="height:' + botH + 'px"></div>';
}

// firstVisible bisects prefix-summed offsets for the first row whose bottom
// edge is below scroll position y.
function firstVisible(rows, y) {
  let lo = 0, hi = rows.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (rows[mid].offset + rows[mid].h > y) hi = mid;
    else lo = mid + 1;
  }
  return lo;
}

function rowHTML(row, hits) {
  if (row.summary) {
    // Summaries carry no text, so an active search always dims them.
    return '<div class="traj-row traj-summary' + (hits ? ' traj-dimrow' : '') + '" data-turn="' + row.turnIndex + '" style="height:' + SUMMARY_H + 'px">' +
      esc(row.steps + ' steps · ' + row.tools + ' tool calls') + '</div>';
  }
  const rec = row.rec;
  const turnAttr = rec.turnIndex ? ' data-turn="' + rec.turnIndex + '"' : '';
  let badge, text;
  if (rec.kind === 'user') { badge = 'you'; text = rec.text; }
  else if (rec.kind === 'assistant') { badge = 'ai'; text = rec.text || (rec.durationMs ? 'thought for ' + formatDuration(rec.durationMs) : 'thinking…'); }
  else if (rec.kind === 'tool') { badge = 'tool'; text = rec.text; }
  else if (rec.kind === 'compaction') { badge = 'compact'; text = rec.text; }
  else { badge = 'sys'; text = rec.text; }
  let meta = '';
  if (rec.kind === 'tool' && rec.durationMs) {
    meta += ' <span class="traj-dim">· ' + esc(formatDuration(rec.durationMs)) + '</span>';
  }
  if (rec.kind === 'assistant') {
    if (rec.text && rec.durationMs) meta += ' <span class="traj-dim">· thought ' + esc(formatDuration(rec.durationMs)) + '</span>';
    const u = rec.usage;
    const total = u ? (u.input || 0) + (u.output || 0) + (u.cache_read || 0) + (u.cache_write || 0) : 0;
    if (total > 0) meta += ' <span class="traj-dim">· ' + esc(formatTokens(total) + ' tok') + '</span>';
  }
  const cls = 'traj-row traj-' + rec.kind + (rec.kind === 'user' ? ' traj-header' : '') + (rec.isError ? ' traj-error' : '') + (hits && !hits.has(rec.id) ? ' traj-dimrow' : '');
  return '<div class="' + cls + '"' + turnAttr + ' data-id="' + esc(rec.id) + '" style="height:' + ROW_H + 'px">' +
    '<span class="traj-badge">' + badge + '</span>' +
    '<span class="traj-text">' + esc(text) + '</span>' + meta +
    (rec.right ? '<span class="traj-right">' + esc(rec.right) + '</span>' : '') +
    '</div>';
}

function oneLine(s) {
  return String(s || '').replace(/\s+/g, ' ').trim().slice(0, 160);
}

function clock(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d)) return '';
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

window.usherTrajectory = { project, render };
