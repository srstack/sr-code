// sr code — reusable dropdown component (Codex GUI style).
//
// makeDropdown({options, value, placeholder, onChange, filterable, allowCustom})
// returns { el, getValue, setValue, setOptions, open, close }.
//
// options: [{id, label}] — label falls back to id. allowCustom adds a
// 'Use "<filter>"' row when the filter text matches no option, so gateways
// with unlisted models stay usable.

export function makeDropdown(opts) {
  const state = {
    options: opts.options || [],
    value: opts.value || '',
    filter: '',
    open: false,
    highlight: -1,
  };

  const el = document.createElement('div');
  el.className = 'dropdown';

  const trigger = document.createElement('button');
  trigger.type = 'button';
  trigger.className = 'dropdown-trigger';
  trigger.setAttribute('aria-haspopup', 'listbox');

  const pop = document.createElement('div');
  pop.className = 'dropdown-pop';
  pop.hidden = true;

  el.appendChild(trigger);
  el.appendChild(pop);

  const labelOf = (id) => {
    const o = state.options.find(o => o.id === id);
    return o ? (o.label || o.id) : '';
  };

  const render = () => {
    const label = labelOf(state.value);
    trigger.innerHTML =
      `<span class="dropdown-value${label ? '' : ' placeholder'}">${escapeHtml(label || opts.placeholder || 'select')}</span>` +
      '<svg class="dropdown-chevron" width="10" height="6" viewBox="0 0 10 6" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M1 1l4 4 4-4"/></svg>';
    trigger.classList.toggle('open', state.open);
  };

  const filtered = () => {
    const f = state.filter.trim().toLowerCase();
    if (!f) return state.options;
    return state.options.filter(o =>
      o.id.toLowerCase().includes(f) || (o.label || '').toLowerCase().includes(f));
  };

  const renderPop = () => {
    const items = filtered();
    const rows = [];
    if (opts.filterable !== false && state.options.length > 6) {
      rows.push(`<input class="dropdown-filter" type="text" placeholder="filter…" value="${escapeHtml(state.filter)}">`);
    }
    if (!items.length) {
      rows.push('<div class="dropdown-empty">no matches</div>');
    }
    items.forEach((o, i) => {
      const sel = o.id === state.value;
      rows.push(
        `<button type="button" class="dropdown-item${sel ? ' selected' : ''}${i === state.highlight ? ' highlight' : ''}" data-id="${escapeHtml(o.id)}" role="option" aria-selected="${sel}">` +
        `<span class="dropdown-item-label">${escapeHtml(o.label || o.id)}</span>` +
        (sel ? '<svg class="dropdown-check" width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8.5l3.5 3.5L13 4.5"/></svg>' : '') +
        `</button>`);
    });
    if (opts.allowCustom && state.filter.trim()) {
      const custom = state.filter.trim();
      if (!state.options.some(o => o.id === custom)) {
        rows.push(
          `<button type="button" class="dropdown-item dropdown-custom" data-custom="${escapeHtml(custom)}">` +
          `<span class="dropdown-item-label">Use "${escapeHtml(custom)}"</span></button>`);
      }
    }
    pop.innerHTML = rows.join('');
    const filterEl = pop.querySelector('.dropdown-filter');
    if (filterEl) {
      filterEl.addEventListener('input', () => {
        state.filter = filterEl.value;
        state.highlight = -1;
        renderPop();
        const f2 = pop.querySelector('.dropdown-filter');
        if (f2) { f2.focus(); f2.setSelectionRange(f2.value.length, f2.value.length); }
      });
      filterEl.addEventListener('keydown', onPopKey);
      filterEl.focus();
    }
    pop.querySelectorAll('.dropdown-item').forEach(item => {
      item.addEventListener('click', () => {
        choose(item.dataset.id !== undefined ? item.dataset.id : item.dataset.custom);
      });
    });
  };

  const onPopKey = (e) => {
    const items = filtered();
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const dir = e.key === 'ArrowDown' ? 1 : -1;
      state.highlight = Math.min(items.length - 1, Math.max(0, state.highlight + dir));
      renderPop();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const custom = state.filter.trim();
      if (state.highlight >= 0 && items[state.highlight]) {
        choose(items[state.highlight].id);
      } else if (items.length === 1) {
        choose(items[0].id);
      } else if (opts.allowCustom && custom) {
        choose(custom);
      }
    } else if (e.key === 'Escape') {
      e.preventDefault();
      api.close();
    }
  };

  const choose = (id) => {
    if (id === undefined || id === '') return;
    state.value = id;
    state.filter = '';
    state.highlight = -1;
    render();
    api.close();
    if (opts.onChange) opts.onChange(id);
  };

  const onDocClick = (e) => {
    if (!el.contains(e.target)) api.close();
  };

  const api = {
    el,
    open() {
      if (state.open) return;
      state.open = true;
      state.filter = '';
      state.highlight = -1;
      pop.hidden = false;
      render();
      renderPop();
      document.addEventListener('click', onDocClick);
    },
    close() {
      if (!state.open) return;
      state.open = false;
      pop.hidden = true;
      render();
      document.removeEventListener('click', onDocClick);
    },
    getValue: () => state.value,
    setValue(v) { state.value = v; render(); },
    setOptions(options, keepValue) {
      state.options = options || [];
      if (!keepValue) {
        // keep the current value when it still exists, else take the first
        if (!state.options.some(o => o.id === state.value)) {
          state.value = state.options.length ? state.options[0].id : '';
        }
      }
      render();
    },
  };

  trigger.addEventListener('click', () => {
    if (state.open) api.close();
    else api.open();
  });
  trigger.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown') {
      e.preventDefault();
      api.open();
    }
  });

  render();
  return api;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}
