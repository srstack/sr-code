// usher SPA: entry point.
// Hash-based routing between session list, detail view, new session, and main chat.

import { closeES, clearListInterval, setEditorUrl } from './state.js';
import './render.js'; // side-effect: sets up marked, render-pill listeners
import { loadSidebar, updateSidebarActive } from './sidebar.js';
import { showList, loadList } from './list.js';
import { showDetail, showNewSession, showMainChat } from './detail.js';
import { pollInteractions } from './interaction.js';
import { initServiceWorker } from './push.js';

window.addEventListener('hashchange', route);

function route() {
  const hash = location.hash || '#/';
  if (hash === '#/' || hash === '') {
    showList();
  } else if (hash === '#/new') {
    showNewSession();
  } else if (hash.startsWith('#/new/')) {
    showNewSession(decodeURIComponent(hash.slice('#/new/'.length)));
  } else if (hash === '#/chat' || hash.startsWith('#/chat/')) {
    const id = hash === '#/chat' ? 'default' : decodeURIComponent(hash.slice('#/chat/'.length));
    showMainChat(id);
  } else if (hash.startsWith('#/s/')) {
    showDetail(decodeURIComponent(hash.slice(4)));
  }
  updateSidebarActive();
}

setInterval(pollInteractions, 2000);
pollInteractions();

setInterval(loadSidebar, 5000);
loadSidebar();

route();

fetch('/api/config')
  .then(r => (r.ok ? r.json() : null))
  .then(c => { if (c) setEditorUrl(c.editor_url || ''); })
  .catch(() => {});

initServiceWorker();
