// usher SPA: entry point.
// Hash-based routing between session list, detail view, new session, and main chat.

import { closeES, clearListInterval, setEditorUrl, loadModelCatalogs } from './state.js';
import './render.js'; // side-effect: sets up marked, render-pill listeners
import { loadSidebar, updateSidebarActive } from './sidebar.js';
import { showList, loadList } from './list.js';
import { showDetail, showNewSession, showMainChat } from './detail.js';
import { pollInteractions } from './interaction.js';
import { initServiceWorker } from './push.js';
import { loadEmbeds, showEmbed, leaveEmbed } from './embedview.js';

window.addEventListener('hashchange', route);

function route() {
  const hash = location.hash || '#/';
  // Embedded agent UIs take over the whole main wrap (no header chrome);
  // the body class is what style.css keys the header-hiding rule off.
  document.body.classList.toggle('embed-route', hash.startsWith('#/ui/'));
  if (!hash.startsWith('#/ui/')) leaveEmbed();
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
  } else if (hash.startsWith('#/ui/')) {
    showEmbed(decodeURIComponent(hash.slice('#/ui/'.length)));
  }
  updateSidebarActive();
}

setInterval(pollInteractions, 2000);
pollInteractions();

setInterval(loadSidebar, 5000);
loadSidebar();

// Embedded agent UIs change rarely (a child flip to ready is the only live
// transition), so a slow 30s poll suffices — no SSE.
setInterval(loadEmbeds, 30000);
loadEmbeds();

route();

// Warm the shared model catalog cache at boot so no picker ever waits.
loadModelCatalogs();

fetch('/api/config')
  .then(r => (r.ok ? r.json() : null))
  .then(c => { if (c) setEditorUrl(c.editor_url || ''); })
  .catch(() => {});

initServiceWorker();
