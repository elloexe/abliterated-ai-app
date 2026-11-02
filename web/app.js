/* Kestrel UI. Plain JS, no build step; libraries are bundled in vendor/. */
'use strict';

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const ICON = {
  copy: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>',
  check: '<svg viewBox="0 0 24 24"><path d="M5 12l5 5L20 7"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/></svg>',
  regen: '<svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-2.6-6.4"/><path d="M21 3v6h-6"/></svg>',
  more: '<svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/></svg>',
  pencil: '<svg viewBox="0 0 24 24"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/></svg>',
  chev: '<svg viewBox="0 0 24 24" class="chev"><path d="M9 6l6 6-6 6"/></svg>',
  bulb: '<svg viewBox="0 0 24 24" style="width:15px;height:15px"><path d="M9 18h6M10 22h4M12 2a7 7 0 0 0-4 12.7V17h8v-2.3A7 7 0 0 0 12 2z"/></svg>',
  tool: {
    web_search: '<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>',
    fetch_url: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/></svg>',
    run_javascript: '<svg viewBox="0 0 24 24"><path d="M8 6l-6 6 6 6M16 6l6 6-6 6"/></svg>',
    run_shell: '<svg viewBox="0 0 24 24"><path d="M4 17l6-5-6-5M12 19h8"/></svg>',
    list_files: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>',
    read_file: '<svg viewBox="0 0 24 24"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6M8 13h8M8 17h5"/></svg>',
    write_file: '<svg viewBox="0 0 24 24"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6M12 18v-6M9 15h6"/></svg>',
    get_datetime: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
  },
};

const state = {
  chats: [],
  chat: null,          // current chat object
  busy: false,         // generating in the current chat
  abort: null,
  config: null,
  defaults: null,
  status: null,
  toolState: {},       // tool_call_id -> {status}
  thinkOpen: {},       // message index -> bool (user toggled)
  liveThinking: null,  // index of message currently thinking
  turnError: null,
  instance: null,
  logSeq: 0,
  logLines: [],
  editing: null,       // index of user message being edited
};

// ---------------- API ----------------
async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: { 'X-App': 'Kestrel' } };
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  const r = await fetch(path, init);
  const ct = r.headers.get('content-type') || '';
  const data = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}

function toast(msg, ms = 2200) {
  const t = $('#toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => (t.hidden = true), ms);
}

async function copyText(text, btn) {
  try { await navigator.clipboard.writeText(text); }
  catch {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand('copy'); ta.remove();
  }
  if (btn) {
    const old = btn.innerHTML;
    btn.innerHTML = btn.classList.contains('copy-btn') ? ICON.check + 'Copied' : ICON.check;
    setTimeout(() => (btn.innerHTML = old), 1400);
  }
}

// ---------------- markdown ----------------
marked.use({ gfm: true, breaks: false });

function renderMarkdown(text) {
  const html = marked.parse(text || '');
  const clean = DOMPurify.sanitize(html, { ADD_ATTR: ['target'] });
  const tpl = document.createElement('template');
  tpl.innerHTML = clean;
  tpl.content.querySelectorAll('a[href]').forEach((a) => { a.target = '_blank'; a.rel = 'noopener noreferrer'; });
  tpl.content.querySelectorAll('pre > code').forEach((code) => {
    const pre = code.parentElement;
    const lang = ((code.className.match(/language-([\w+#.-]+)/) || [])[1] || '').toLowerCase();
    try {
      if (lang && hljs.getLanguage(lang)) hljs.highlightElement(code);
      else if (code.textContent.length < 20000) { const r = hljs.highlightAuto(code.textContent); code.innerHTML = r.value; code.classList.add('hljs'); }
    } catch { /* leave unhighlighted */ }
    const wrap = document.createElement('div');
    wrap.className = 'codeblock';
    wrap.innerHTML = `<div class="code-head"><span>${esc(lang || 'code')}</span><button class="copy-btn" data-copy-code>${ICON.copy}Copy</button></div>`;
    pre.replaceWith(wrap);
    wrap.appendChild(pre);
  });
  return tpl.innerHTML;
}

// ---------------- formatting ----------------
function fmtSecs(s) {
  if (!s || s < 1) return 'a moment';
  if (s < 60) return `${Math.round(s)}s`;
  return `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`;
}

function fmtStats(st) {
  if (!st || !st.tokens) return '';
  const parts = [`<b>${st.tok_per_sec.toFixed(1)}</b> tok/s`, `${st.tokens} tokens`];
  if (st.draft_n > 0) parts.push(`<b>${Math.round(st.drafted_pct)}%</b> drafted`);
  const tip = [`Prompt: ${st.prompt_tokens} tokens`, `Generated: ${st.tokens} tokens in ${(st.predicted_ms / 1000).toFixed(1)}s`];
  if (st.draft_n > 0) tip.push(`0.8B drafted ${st.draft_n}, 9B accepted ${st.draft_accepted} (${Math.round(100 * st.draft_accepted / st.draft_n)}% acceptance)`);
  if (st.mode) tip.push(`Mode: ${st.mode}`);
  if (st.steps > 1) tip.push(`Agent steps: ${st.steps}`);
  return `<span class="stats" title="${esc(tip.join('\n'))}">${parts.join(' · ')}</span>`;
}

function prettyArgs(a) {
  try { return JSON.stringify(JSON.parse(a), null, 2); } catch { return a || ''; }
}

function toolSubtitle(name, argsStr) {
  let a = {};
  try { a = JSON.parse(argsStr || '{}'); } catch { /* ignore */ }
  return a.query || a.url || a.path || a.command || (a.code ? a.code.split('\n')[0] : '') || '';
}

// ---------------- rendering ----------------
// The thread is a list of turns: one user message, then one assistant turn
// made of every assistant/tool message until the next user message.
function groupTurns(msgs) {
  const turns = [];
  let cur = null;
  msgs.forEach((m, i) => {
    if (m.role === 'user') {
      turns.push({ type: 'user', index: i, msg: m });
      cur = null;
    } else if (m.role === 'assistant' || m.role === 'tool') {
      if (!cur) { cur = { type: 'assistant', items: [] }; turns.push(cur); }
      cur.items.push({ index: i, msg: m });
    }
  });
  return turns;
}

function renderThread() {
  const thread = $('#thread');
  const msgs = state.chat ? state.chat.messages : [];
  $('#empty').hidden = msgs.length > 0;
  const turns = groupTurns(msgs);
  const lastAssistant = turns.map((t) => t.type).lastIndexOf('assistant');
  const html = turns.map((t, ti) => (t.type === 'user' ? renderUser(t, ti === turns.length - 1) : renderAssistant(t, ti === lastAssistant, ti === turns.length - 1))).join('');
  let tail = '';
  if (state.turnError) tail += `<div class="error-box">${esc(state.turnError)}</div>`;
  if (state.busy && (turns.length === 0 || turns[turns.length - 1].type === 'user')) {
    tail += `<div class="turn assistant"><span class="shimmer">${state.status && state.status.main.state !== 'ready' ? 'Waiting for the model to load…' : 'Working…'}</span></div>`;
  }
  thread.innerHTML = html + tail;
}

function renderUser(t, isLast) {
  if (state.editing === t.index) {
    return `<div class="turn user"><div class="edit-box"><textarea id="edit-input">${esc(t.msg.content)}</textarea>
      <div class="row"><button class="btn ghost small" data-act="edit-cancel">Cancel</button><button class="btn small" data-act="edit-send" data-index="${t.index}">Send</button></div></div></div>`;
  }
  return `<div class="turn user${isLast ? ' last' : ''}" data-index="${t.index}">
    <div class="bubble">${esc(t.msg.content)}</div>
    <div class="msg-actions">
      <button class="icon-btn" title="Copy" data-act="copy-user" data-index="${t.index}">${ICON.copy}</button>
      ${state.busy ? '' : `<button class="icon-btn" title="Edit and resend" data-act="edit" data-index="${t.index}">${ICON.edit}</button>`}
    </div></div>`;
}

function renderAssistant(t, isLastAssistant, isLastTurn) {
  const msgs = state.chat.messages;
  const results = {};
  t.items.forEach(({ msg }) => { if (msg.role === 'tool') results[msg.tool_call_id] = msg; });
  let body = '';
  let lastStats = null;
  let text = '';
  t.items.forEach(({ index, msg }) => {
    if (msg.role !== 'assistant') return;
    const live = state.busy && isLastTurn && index === msgs.length - 1;
    if (msg.reasoning_content) body += renderThinking(index, msg, live);
    (msg.tool_calls || []).forEach((tc) => { body += renderTool(tc, results[tc.id]); });
    if (msg.content) {
      body += `<div class="md${live ? ' cursor' : ''}">${renderMarkdown(msg.content)}</div>`;
      text += (text ? '\n\n' : '') + msg.content;
    } else if (live && !msg.reasoning_content && !(msg.tool_calls || []).length) {
      body += '<span class="shimmer">Working…</span>';
    }
    if (msg.meta && msg.meta.stopped) body += '<div class="stopped-note">Stopped</div>';
    if (msg.meta && msg.meta.stats) lastStats = msg.meta.stats;
  });
  const showActions = !(state.busy && isLastTurn);
  const actions = showActions ? `<div class="msg-actions">
      ${text ? `<button class="icon-btn" title="Copy" data-act="copy-text" data-text="${esc(text)}">${ICON.copy}</button>` : ''}
      ${isLastAssistant && !state.busy ? `<button class="icon-btn" title="Regenerate" data-act="regen">${ICON.regen}</button>` : ''}
      ${fmtStats(lastStats)}
    </div>` : '';
  return `<div class="turn assistant${isLastTurn ? ' last' : ''}">${body}${actions}</div>`;
}

function renderThinking(index, msg, live) {
  const thinking = live && state.liveThinking === index;
  const secs = msg.meta && msg.meta.thought_secs;
  const label = thinking ? '<span class="shimmer">Thinking…</span>' : `Thought for ${fmtSecs(secs)}`;
  const open = state.thinkOpen[index] !== undefined ? state.thinkOpen[index] : thinking;
  return `<details class="think" data-think="${index}"${open ? ' open' : ''}>
    <summary>${ICON.bulb}${label}${ICON.chev}</summary>
    <div class="think-body">${esc(msg.reasoning_content)}</div></details>`;
}

function renderTool(tc, result) {
  const name = tc.function.name;
  const st = state.toolState[tc.id];
  let status = result ? (result.meta && result.meta.tool_status) || 'ok' : (st && st.status) ? st.status : (state.busy ? 'running' : 'error');
  const labels = { running: 'Running', approval: 'Needs approval', ok: 'Done', error: 'Error', denied: 'Denied' };
  const icon = ICON.tool[name] || ICON.tool.run_shell;
  const open = status === 'approval' || (st && st.open);
  return `<details class="tool-card" data-tool="${esc(tc.id)}"${open ? ' open' : ''}>
    <summary>${status === 'running' ? '<span class="spin"></span>' : icon}<span class="tname">${esc(name)}</span>
      <span class="tsub">${esc(toolSubtitle(name, tc.function.arguments))}</span>
      <span class="tstat ${status}">${labels[status] || status}</span></summary>
    <div class="tbody">
      <div class="tlabel">Input</div><pre>${esc(prettyArgs(tc.function.arguments))}</pre>
      ${result ? `<div class="tlabel">Result</div><pre>${esc(result.content)}</pre>` : ''}
    </div>
    ${status === 'approval' ? `<div class="approval"><span>Allow <b>${esc(name)}</b> to run?</span>
      <button class="btn ghost small" data-act="deny" data-id="${esc(tc.id)}">Deny</button>
      <button class="btn accent small" data-act="allow" data-id="${esc(tc.id)}">Allow</button></div>` : ''}
  </details>`;
}

// Re-render on the next frame, keeping the scroll pinned to the bottom when
// the user was already there.
let rafPending = false;
function scheduleRender() {
  if (rafPending) return;
  rafPending = true;
  requestAnimationFrame(() => {
    rafPending = false;
    const box = $('#messages');
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
    const thinkScroll = {};
    $$('.think-body').forEach((el) => { const d = el.parentElement.dataset.think; thinkScroll[d] = el.scrollHeight - el.scrollTop - el.clientHeight < 30; });
    renderThread();
    $$('.think-body').forEach((el) => { const d = el.parentElement.dataset.think; if (thinkScroll[d] !== false) el.scrollTop = el.scrollHeight; });
    if (atBottom) box.scrollTop = box.scrollHeight;
  });
}

function scrollToBottom() {
  const box = $('#messages');
  box.scrollTop = box.scrollHeight;
}

// ---------------- sidebar ----------------
function dateGroup(ms) {
  const d = new Date(ms);
  const now = new Date();
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  if (ms >= start) return 'Today';
  if (ms >= start - day) return 'Yesterday';
  if (ms >= start - 7 * day) return 'Previous 7 days';
  if (ms >= start - 30 * day) return 'Previous 30 days';
  return d.toLocaleString(undefined, { month: 'long', year: 'numeric' });
}

async function loadChats() {
  const q = $('#search').value.trim();
  try { state.chats = await api('/api/chats' + (q ? '?q=' + encodeURIComponent(q) : '')); } catch { return; }
  renderChatList();
}

function renderChatList() {
  const list = $('#chat-list');
  if (state.chat) {
    const cur = state.chats.find((c) => c.id === state.chat.id);
    if (cur && cur.title !== state.chat.title) state.chat.title = cur.title;
    $('#chat-title').textContent = state.chat.title;
  }
  const q = $('#search').value.trim();
  if (!state.chats.length) {
    list.innerHTML = `<div class="empty-list">${q ? 'No chats match.' : 'No chats yet.'}</div>`;
    return;
  }
  let html = '';
  let group = '';
  for (const c of state.chats) {
    const g = dateGroup(c.updated);
    if (g !== group) { html += `<div class="group-label">${esc(g)}</div>`; group = g; }
    const active = state.chat && state.chat.id === c.id;
    const busy = active && state.busy;
    html += `<div class="chat-item${active ? ' active' : ''}" data-id="${esc(c.id)}" title="${esc(c.preview || c.title)}">
      <span class="t">${esc(c.title)}${q && c.preview ? `<span class="p">${esc(c.preview)}</span>` : ''}</span>
      ${busy ? '<span class="busy-dot"></span>' : ''}
      <button class="icon-btn more" data-menu="${esc(c.id)}" aria-label="Chat options">${ICON.more}</button></div>`;
  }
  list.innerHTML = html;
}

function closeMenus() { $$('.menu').forEach((m) => m.remove()); $$('.chat-item.menu-open').forEach((e) => e.classList.remove('menu-open')); }

function openChatMenu(id, anchor) {
  closeMenus();
  const r = anchor.getBoundingClientRect();
  const m = document.createElement('div');
  m.className = 'menu';
  m.innerHTML = `<button data-m="rename">${ICON.pencil}Rename</button><button data-m="delete" class="danger">${ICON.trash}Delete</button>`;
  m.style.left = Math.min(r.left, innerWidth - 180) + 'px';
  m.style.top = (r.bottom + 4) + 'px';
  document.body.appendChild(m);
  anchor.closest('.chat-item').classList.add('menu-open');
  m.addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    closeMenus();
    if (b.dataset.m === 'rename') startRename(id);
    if (b.dataset.m === 'delete') deleteChat(id);
  });
}

function startRename(id) {
  const item = $(`.chat-item[data-id="${CSS.escape(id)}"]`);
  if (!item) return;
  const chat = state.chats.find((c) => c.id === id);
  const input = document.createElement('input');
  input.className = 'rename';
  input.value = chat ? chat.title : '';
  item.querySelector('.t').replaceWith(input);
  input.focus(); input.select();
  let done = false;
  const finish = async (save) => {
    if (done) return; done = true;
    const title = input.value.trim();
    if (save && title) {
      try {
        await api(`/api/chats/${id}`, { method: 'PATCH', body: { title } });
        if (state.chat && state.chat.id === id) { state.chat.title = title; $('#chat-title').textContent = title; }
      } catch (e) { toast(e.message); }
    }
    loadChats();
  };
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') finish(true); if (e.key === 'Escape') finish(false); });
  input.addEventListener('blur', () => finish(true));
  input.addEventListener('click', (e) => e.stopPropagation());
}

async function deleteChat(id) {
  const chat = state.chats.find((c) => c.id === id);
  if (!confirm(`Delete "${chat ? chat.title : 'this chat'}"? This cannot be undone.`)) return;
  try { await api(`/api/chats/${id}`, { method: 'DELETE' }); } catch (e) { toast(e.message); }
  if (state.chat && state.chat.id === id) newChat();
  loadChats();
}

async function openChat(id) {
  if (state.busy && state.chat && state.chat.id !== id) {
    // Leaving a chat mid-reply stops it; the partial reply is saved.
    stopGeneration();
  }
  try {
    const r = await api('/api/chats/' + id);
    state.chat = r.chat;
  } catch (e) { toast(e.message); return; }
  state.toolState = {}; state.thinkOpen = {}; state.turnError = null; state.editing = null;
  $('#chat-title').textContent = state.chat.title;
  renderThread();
  renderChatList();
  scrollToBottom();
  $('#input').focus();
}

function newChat() {
  if (state.busy) stopGeneration();
  state.chat = null; state.toolState = {}; state.thinkOpen = {}; state.turnError = null; state.editing = null;
  $('#chat-title').textContent = 'New chat';
  renderThread();
  renderChatList();
  $('#input').value = '';
  autoGrow();
  $('#input').focus();
}

// ---------------- sending ----------------
function setBusy(b) {
  state.busy = b;
  const send = $('#send');
  send.classList.toggle('busy', b);
  send.title = b ? 'Stop' : 'Send (Enter)';
  updateSendEnabled();
}

function updateSendEnabled() {
  $('#send').disabled = !state.busy && !$('#input').value.trim();
}

async function send(opts) {
  if (state.busy) return;
  state.turnError = null;
  if (!state.chat) {
    try { state.chat = await api('/api/chats', { method: 'POST' }); }
    catch (e) { toast(e.message); return; }
    $('#chat-title').textContent = state.chat.title;
  }
  const body = {
    thinking: $('#chip-thinking').getAttribute('aria-pressed') === 'true',
    tools: $('#chip-tools').getAttribute('aria-pressed') === 'true',
    ...opts,
  };
  // Optimistic update so the message appears instantly.
  const msgs = state.chat.messages;
  if (opts.regenerate) {
    let last = msgs.map((m) => m.role).lastIndexOf('user');
    msgs.splice(last + 1);
  } else if (opts.edit_index !== undefined) {
    msgs.splice(opts.edit_index);
    msgs.push({ role: 'user', content: opts.content });
  } else {
    msgs.push({ role: 'user', content: opts.content });
  }
  state.editing = null;
  setBusy(true);
  renderThread();
  scrollToBottom();
  renderChatList();

  const ctrl = new AbortController();
  state.abort = ctrl;
  const chatId = state.chat.id;
  try {
    const r = await fetch(`/api/chats/${chatId}/send`, {
      method: 'POST', signal: ctrl.signal,
      headers: { 'Content-Type': 'application/json', 'X-App': 'Kestrel' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch { /* ignore */ }
      throw new Error(msg);
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, i);
        buf = buf.slice(i + 2);
        for (const line of chunk.split('\n')) {
          if (line.startsWith('data: ')) {
            try { handleEvent(JSON.parse(line.slice(6)), chatId); } catch (e) { console.error(e); }
          }
        }
      }
    }
  } catch (e) {
    if (e.name !== 'AbortError') state.turnError = e.message;
  } finally {
    state.abort = null;
    state.liveThinking = null;
    setBusy(false);
    // Reload the saved chat so the view matches what is on disk.
    if (state.chat && state.chat.id === chatId) {
      try { const r = await api('/api/chats/' + chatId); state.chat = r.chat; } catch { /* keep local */ }
      scheduleRender();
    }
    loadChats();
    setTimeout(loadChats, 2500);
    setTimeout(() => refreshTitle(chatId), 6000);
    setTimeout(() => refreshTitle(chatId), 15000);
  }
}

async function refreshTitle(id) {
  if (!state.chat || state.chat.id !== id) { loadChats(); return; }
  try {
    const r = await api('/api/chats/' + id);
    if (state.chat && state.chat.id === id && r.chat.title !== state.chat.title) {
      state.chat.title = r.chat.title;
      $('#chat-title').textContent = r.chat.title;
    }
  } catch { /* ignore */ }
  loadChats();
}

function handleEvent(ev, chatId) {
  if (!state.chat || state.chat.id !== chatId) return;
  const msgs = state.chat.messages;
  const last = msgs[msgs.length - 1];
  switch (ev.type) {
    case 'chat':
      state.chat = ev.chat;
      break;
    case 'assistant_start':
      msgs.push({ role: 'assistant', content: '', reasoning_content: '', meta: {} });
      break;
    case 'reasoning':
      if (last && last.role === 'assistant') {
        last.reasoning_content += ev.text;
        if (state.liveThinking === null) state.liveThinking = msgs.length - 1;
      }
      break;
    case 'thinking_done':
      if (last && last.role === 'assistant') { last.meta = last.meta || {}; last.meta.thought_secs = ev.secs; }
      state.liveThinking = null;
      break;
    case 'content':
      if (last && last.role === 'assistant') last.content += ev.text;
      break;
    case 'assistant_end':
      if (last && last.role === 'assistant') {
        msgs[msgs.length - 1] = ev.message;
        ev.message.meta = ev.message.meta || {};
        ev.message.meta.stats = ev.stats;
      }
      state.liveThinking = null;
      break;
    case 'tool_call':
      state.toolState[ev.id] = { status: ev.needs_approval ? 'approval' : 'running' };
      if (ev.needs_approval) notifyAttention();
      break;
    case 'tool_running':
      state.toolState[ev.id] = { status: 'running', open: true };
      break;
    case 'tool_result':
      msgs.push({ role: 'tool', tool_call_id: ev.id, name: ev.name, content: ev.result, meta: { tool_status: ev.status } });
      if (state.toolState[ev.id]) state.toolState[ev.id].status = ev.status;
      break;
    case 'error':
      state.turnError = ev.message;
      break;
    default:
      break;
  }
  scheduleRender();
}

function notifyAttention() {
  if (document.hidden) document.title = '● Approval needed — Kestrel';
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) document.title = 'Kestrel'; });

async function stopGeneration() {
  if (!state.chat) return;
  try { await api(`/api/chats/${state.chat.id}/stop`, { method: 'POST' }); } catch { /* ignore */ }
  // The stream ends by itself once the server stops; abort as a fallback.
  const ctrl = state.abort;
  setTimeout(() => { if (ctrl && state.abort === ctrl) ctrl.abort(); }, 3000);
}

async function approve(id, allow) {
  try {
    await api(`/api/chats/${state.chat.id}/approve`, { method: 'POST', body: { id, allow } });
    state.toolState[id] = { status: allow ? 'running' : 'denied', open: allow };
    scheduleRender();
  } catch (e) { toast(e.message); }
}

// ---------------- composer ----------------
function autoGrow() {
  const ta = $('#input');
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight, 260) + 'px';
  updateSendEnabled();
}

function submitComposer() {
  if (state.busy) { stopGeneration(); return; }
  const text = $('#input').value;
  if (!text.trim()) return;
  $('#input').value = '';
  autoGrow();
  send({ content: text });
}

function setChip(id, on) { $(id).setAttribute('aria-pressed', on ? 'true' : 'false'); }

async function toggleChip(id, key) {
  const on = $(id).getAttribute('aria-pressed') !== 'true';
  setChip(id, on);
  if (state.config) {
    state.config[key] = on;
    try { await api('/api/config', { method: 'PUT', body: state.config }); } catch { /* ignore */ }
  }
}

// ---------------- status ----------------
function dotClass(s) {
  if (!s) return '';
  return { ready: 'green', starting: 'yellow', failed: 'red', stopped: 'red' }[s.state] || '';
}

function applyStatus(st) {
  state.status = st;
  if (state.instance && st.instance !== state.instance) {
    // A new process took over (after setup); reload to pick it up cleanly.
    location.reload();
    return;
  }
  state.instance = st.instance;
  const m = st.main, u = st.util;
  $('#dot-main').className = 'dot ' + dotClass(m);
  $('#dot-util').className = 'dot ' + dotClass(u);
  const mainLabel = m.state === 'ready' ? m.mode : m.state === 'starting' ? 'loading…' : m.state === 'failed' ? 'failed' : 'stopped';
  $('#mode-main').textContent = mainLabel;
  $('#mode-util').textContent = u.state === 'ready' ? 'CPU' : u.state === 'starting' ? 'loading…' : u.state;
  $('#model-status').title = [m.detail, u.detail].filter(Boolean).join('\n') || 'Model status';
  const badge = $('#mode-badge');
  badge.textContent = m.state === 'ready' ? m.mode : m.state === 'starting' ? (m.detail || 'Loading…') : m.state === 'failed' ? 'Model failed — see Settings › Log' : '';
  badge.classList.toggle('warn', m.state !== 'ready' || m.modeId !== 'spec');
  badge.title = m.detail || '';
  $('#ctx-hint').textContent = m.state === 'ready' ? `${Math.round(m.ctx / 1024)}k context` : '';
  // Settings › Models
  $('#s-dot-main').className = 'dot ' + dotClass(m);
  $('#s-dot-util').className = 'dot ' + dotClass(u);
  $('#s-main-info').textContent = `${m.state}${m.mode ? ' · ' + m.mode : ''}${m.detail ? ' · ' + m.detail : ''} · port ${m.port || '-'}`;
  $('#s-util-info').textContent = `${u.state}${u.mode ? ' · ' + u.mode : ''}${u.detail ? ' · ' + u.detail : ''} · port ${u.port || '-'}`;
  $('#about-version').textContent = st.version;
  $('#about-dir').textContent = 'App folder: ' + st.appDir;

  const needSetup = !st.installed || (st.setup && (st.setup.running || st.setup.done || st.setup.error));
  if (needSetup) showSetup(st);
  else if (!$('#setup').hidden) { $('#setup').hidden = true; }
}

async function pollStatus() {
  try { applyStatus(await api('/api/status')); } catch { /* server restarting */ }
}

// ---------------- setup ----------------
let setupPlanCache = null;
let setupRendered = false;

async function showSetup(st) {
  $('#setup').hidden = false;
  const s = st.setup || {};
  if (s.running || s.done || s.error) { renderSetupProgress(s); return; }
  if (setupRendered) return;
  setupRendered = true;
  try { setupPlanCache = await api('/api/setup/plan'); } catch (e) { $('#setup-body').innerHTML = `<div class="error-box">${esc(e.message)}</div>`; return; }
  renderSetupPlan();
}

function gb(n) { return (n / 1073741824).toFixed(2) + ' GB'; }

function renderSetupPlan() {
  const p = setupPlanCache;
  const body = $('#setup-body');
  if (!p.manifestReady) {
    body.innerHTML = `<p>This is a development build without a release manifest. Put the files in place by hand:</p>
      <div class="setup-log">${esc(p.appDir)}\\bin\\llama-server.exe (+ CUDA DLLs)\n${esc(p.appDir)}\\models\\Huihui-Qwen3.5-9B-abliterated.i1-Q4_K_M.gguf\n${esc(p.appDir)}\\models\\Qwen3.5-0.8B-Q8_0.gguf</div>
      <div class="setup-actions"><button class="btn" onclick="location.reload()">Check again</button></div>`;
    return;
  }
  const found = p.found || [], missing = p.missing || [];
  const rows = found.map((f) => `<div><span class="ok">✓</span><span class="n" title="${esc(f.path)}">${esc(f.name)}</span><span class="muted">${gb(f.size)}</span></div>`)
    .concat(missing.map((f) => `<div><span class="miss">○</span><span class="n">${esc(f.name)}</span><span class="muted">${gb(f.size)}</span></div>`)).join('');
  const all = missing.length === 0;
  body.innerHTML = `
    <p class="small">${all ? 'All release files were found. Kestrel will check every file with SHA-256, join the model parts and unpack the engine.' :
      `Kestrel needs the release files from <a href="${esc(p.releaseURL)}" target="_blank" rel="noopener">${esc(p.tag)}</a>. Put them next to Kestrel.exe or in your Downloads folder, or let Kestrel download the missing ${gb(p.missingBytes)} once.`}</p>
    <div class="files">${rows}</div>
    <label class="opt"><input type="radio" name="target" value="install" ${p.isInstalledAt ? '' : 'checked'}><span>Install for this user (recommended)<small>${esc(p.defaultTarget)} · adds Start menu and desktop shortcuts</small></span></label>
    <label class="opt"><input type="radio" name="target" value="here" ${p.isInstalledAt ? 'checked' : ''}><span>Portable: set up in this folder<small>${esc(p.appDir)}</small></span></label>
    <label class="opt"><input type="checkbox" id="setup-delete" checked><span>Delete the downloaded part files afterwards<small>Saves about 7 GB of disk space.</small></span></label>
    <div class="setup-actions">
      <button class="btn ghost" id="setup-recheck">Check again</button>
      ${all ? '<button class="btn accent" id="setup-go">Install</button>' : `<button class="btn accent" id="setup-dl">Download ${gb(p.missingBytes)} and install</button>`}
    </div>
    <p class="muted small">Needs about 7 GB free in the install folder.</p>`;
  $('#setup-recheck').onclick = async () => { setupPlanCache = await api('/api/setup/plan'); renderSetupPlan(); };
  const start = async (download) => {
    const where = ($('input[name=target]:checked') || {}).value;
    const req = { download, delete_source: $('#setup-delete').checked, shortcuts: where === 'install', target: where === 'install' ? p.defaultTarget : '' };
    try { await api('/api/setup/start', { method: 'POST', body: req }); } catch (e) { toast(e.message); return; }
    pollStatus();
  };
  if ($('#setup-go')) $('#setup-go').onclick = () => start(false);
  if ($('#setup-dl')) $('#setup-dl').onclick = () => start(true);
}

function renderSetupProgress(s) {
  const pct = s.total ? Math.min(100, (100 * s.bytes) / s.total) : 0;
  const body = $('#setup-body');
  let actions = '';
  if (s.done) actions = '<div class="setup-actions"><button class="btn accent" id="setup-finish">Start Kestrel</button></div>';
  if (s.error) actions = `<div class="error-box">${esc(s.error)}</div><div class="setup-actions"><button class="btn" id="setup-back">Back</button></div>`;
  body.innerHTML = `
    <div class="small"><b>${esc(s.done ? 'Installed' : s.error ? 'Setup stopped' : s.step || 'Starting…')}</b>
      ${s.running && s.total ? `<span class="muted"> · ${gb(s.bytes)} of ${gb(s.total)}</span>` : ''}</div>
    <div class="progress"><div style="width:${s.done ? 100 : pct}%"></div></div>
    <div class="setup-log" id="setup-log">${esc((s.log || []).join('\n'))}</div>${actions}`;
  const lg = $('#setup-log'); lg.scrollTop = lg.scrollHeight;
  if ($('#setup-finish')) $('#setup-finish').onclick = finishSetup;
  if ($('#setup-back')) $('#setup-back').onclick = () => { setupRendered = false; location.reload(); };
}

async function finishSetup() {
  try {
    const r = await api('/api/setup/finish', { method: 'POST' });
    if (r.relaunch) {
      $('#setup-body').innerHTML = '<p><span class="spin" style="display:inline-block;vertical-align:middle"></span> Starting Kestrel from its install folder…</p>';
      // The installed copy takes over the same address; applyStatus reloads
      // when it sees the new instance.
    } else {
      location.reload();
    }
  } catch (e) { toast(e.message); }
}

// ---------------- settings ----------------
function openSettings(tab) {
  fillSettings();
  $('#settings').hidden = false;
  selectTab(tab || 'general');
  $('#settings-msg').textContent = '';
}

function closeSettings() { $('#settings').hidden = true; }

function selectTab(name) {
  $$('.tabs [role=tab]').forEach((b) => b.setAttribute('aria-selected', b.dataset.tab === name ? 'true' : 'false'));
  $$('.tab-panels [data-panel]').forEach((p) => (p.hidden = p.dataset.panel !== name));
  if (name === 'logs') { renderLogs(); const v = $('#log-view'); v.scrollTop = v.scrollHeight; }
}

function fillSettings() {
  const c = state.config;
  if (!c) return;
  $('#s-system').value = c.system_prompt;
  $('#s-theme').value = c.theme;
  $('#s-temp').value = c.temperature;
  $('#s-topp').value = c.top_p;
  $('#s-topk').value = c.top_k;
  $('#s-maxtok').value = c.max_tokens;
  $('#s-autoapprove').checked = c.auto_approve;
  $('#s-steps').value = c.max_steps;
  $('#s-shell').value = c.shell_timeout_sec;
  $('#s-draft').checked = c.use_draft;
  $('#s-ctx').value = String(c.context_size);
  $('#s-threads').value = c.util_threads;
  updateOutputs();
}

function updateOutputs() {
  $('#o-temp').textContent = Number($('#s-temp').value).toFixed(2);
  $('#o-topp').textContent = Number($('#s-topp').value).toFixed(2);
  $('#o-topk').textContent = $('#s-topk').value;
}

function readSettings() {
  const c = { ...state.config };
  c.system_prompt = $('#s-system').value;
  c.theme = $('#s-theme').value;
  c.temperature = Number($('#s-temp').value);
  c.top_p = Number($('#s-topp').value);
  c.top_k = Number($('#s-topk').value);
  c.max_tokens = Number($('#s-maxtok').value) || c.max_tokens;
  c.auto_approve = $('#s-autoapprove').checked;
  c.max_steps = Number($('#s-steps').value) || c.max_steps;
  c.shell_timeout_sec = Number($('#s-shell').value) || c.shell_timeout_sec;
  c.use_draft = $('#s-draft').checked;
  c.context_size = Number($('#s-ctx').value);
  c.util_threads = Number($('#s-threads').value) || c.util_threads;
  return c;
}

async function saveSettings(restart) {
  const before = state.config;
  const c = readSettings();
  try {
    const r = await api('/api/config', { method: 'PUT', body: c });
    state.config = r.config;
  } catch (e) { $('#settings-msg').textContent = e.message; return false; }
  applyTheme(state.config.theme);
  const modelChange = before.use_draft !== c.use_draft || before.context_size !== c.context_size || before.util_threads !== c.util_threads;
  if (restart || modelChange) {
    try { await api('/api/restart', { method: 'POST' }); toast('Restarting models…'); } catch (e) { toast(e.message); }
  } else {
    toast('Settings saved');
  }
  return true;
}

function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  $('#hl-dark').disabled = t !== 'dark';
  $('#hl-light').disabled = t === 'dark';
  try { localStorage.setItem('kestrel-theme', t); } catch { /* ignore */ }
}

// ---------------- logs ----------------
async function pollLogs() {
  try {
    const lines = await api('/api/logs?since=' + state.logSeq);
    if (lines.length) {
      state.logSeq = lines[lines.length - 1].seq;
      state.logLines.push(...lines);
      if (state.logLines.length > 3000) state.logLines.splice(0, state.logLines.length - 3000);
      if (!$('#settings').hidden && !$('[data-panel=logs]').hidden) renderLogs();
    }
  } catch { /* ignore */ }
}

function renderLogs() {
  const f = $('#log-filter').value;
  const v = $('#log-view');
  const atBottom = v.scrollHeight - v.scrollTop - v.clientHeight < 40;
  v.innerHTML = state.logLines.filter((l) => !f || l.source === f).slice(-1500)
    .map((l) => `<span class="tm">${l.time}</span> <span class="src">[${esc(l.source)}]</span> ${esc(l.text)}`).join('\n');
  if ($('#log-follow').checked && (atBottom || true)) v.scrollTop = v.scrollHeight;
}

// ---------------- events ----------------
function bind() {
  $('#composer').addEventListener('submit', (e) => { e.preventDefault(); submitComposer(); });
  $('#input').addEventListener('input', autoGrow);
  $('#input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); if (!state.busy) submitComposer(); }
  });
  $('#chip-thinking').addEventListener('click', () => toggleChip('#chip-thinking', 'thinking'));
  $('#chip-tools').addEventListener('click', () => toggleChip('#chip-tools', 'tools'));
  $('#new-chat').addEventListener('click', newChat);
  $('#collapse-side').addEventListener('click', () => $('#app').classList.add('side-hidden'));
  $('#show-side').addEventListener('click', () => $('#app').classList.remove('side-hidden'));
  $('#toggle-theme').addEventListener('click', async () => {
    const t = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
    applyTheme(t);
    if (state.config) { state.config.theme = t; try { await api('/api/config', { method: 'PUT', body: state.config }); } catch { /* ignore */ } }
  });
  let searchT;
  $('#search').addEventListener('input', () => { clearTimeout(searchT); searchT = setTimeout(loadChats, 200); });

  $('#chat-list').addEventListener('click', (e) => {
    const more = e.target.closest('[data-menu]');
    if (more) { e.stopPropagation(); openChatMenu(more.dataset.menu, more); return; }
    const item = e.target.closest('.chat-item');
    if (item && !e.target.closest('input')) openChat(item.dataset.id);
  });
  $('#chat-list').addEventListener('dblclick', (e) => { const item = e.target.closest('.chat-item'); if (item) startRename(item.dataset.id); });
  document.addEventListener('click', (e) => { if (!e.target.closest('.menu')) closeMenus(); });

  $('.suggestions').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-prompt]');
    if (!b) return;
    $('#input').value = b.dataset.prompt;
    autoGrow();
    $('#input').focus();
  });

  $('#thread').addEventListener('click', (e) => {
    const cc = e.target.closest('[data-copy-code]');
    if (cc) { copyText(cc.closest('.codeblock').querySelector('code').innerText, cc); return; }
    const b = e.target.closest('[data-act]');
    if (!b) return;
    const act = b.dataset.act;
    const idx = Number(b.dataset.index);
    if (act === 'copy-user') copyText(state.chat.messages[idx].content, b);
    if (act === 'copy-text') copyText(b.dataset.text, b);
    if (act === 'regen') send({ regenerate: true });
    if (act === 'edit') { state.editing = idx; renderThread(); const ta = $('#edit-input'); ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); ta.style.height = ta.scrollHeight + 'px'; }
    if (act === 'edit-cancel') { state.editing = null; renderThread(); }
    if (act === 'edit-send') { const v = $('#edit-input').value; if (v.trim()) send({ content: v, edit_index: idx }); }
    if (act === 'allow') approve(b.dataset.id, true);
    if (act === 'deny') approve(b.dataset.id, false);
  });
  $('#thread').addEventListener('keydown', (e) => {
    if (e.target.id === 'edit-input' && e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); $('[data-act=edit-send]').click(); }
    if (e.target.id === 'edit-input' && e.key === 'Escape') { state.editing = null; renderThread(); }
  });
  // Remember which thinking blocks / tool cards the user opened or closed.
  // (Clicks, not 'toggle' events: re-rendering <details open> fires those too.)
  $('#thread').addEventListener('click', (e) => {
    const sum = e.target.closest('summary');
    if (!sum) return;
    const d = sum.parentElement;
    const willOpen = !d.open;
    if (d.dataset.think !== undefined) state.thinkOpen[d.dataset.think] = willOpen;
    if (d.dataset.tool) { const s = state.toolState[d.dataset.tool] || (state.toolState[d.dataset.tool] = {}); s.open = willOpen; }
  });

  // Settings
  $('#open-settings').addEventListener('click', () => openSettings());
  $('#model-status').addEventListener('click', () => openSettings('models'));
  $$('#settings [data-close]').forEach((b) => b.addEventListener('click', closeSettings));
  $('#settings').addEventListener('mousedown', (e) => { if (e.target.id === 'settings') closeSettings(); });
  $$('.tabs [role=tab]').forEach((b) => b.addEventListener('click', () => selectTab(b.dataset.tab)));
  ['#s-temp', '#s-topp', '#s-topk'].forEach((id) => $(id).addEventListener('input', updateOutputs));
  $('#save-settings').addEventListener('click', async () => { if (await saveSettings(false)) closeSettings(); });
  $('#restart-models').addEventListener('click', async () => { await saveSettings(true); });
  $('#reset-system').addEventListener('click', () => { $('#s-system').value = state.defaults.system_prompt; });
  $('#reset-sampling').addEventListener('click', () => {
    const d = state.defaults;
    $('#s-temp').value = d.temperature; $('#s-topp').value = d.top_p; $('#s-topk').value = d.top_k; $('#s-maxtok').value = d.max_tokens;
    updateOutputs();
  });
  $('#log-filter').addEventListener('change', renderLogs);
  $('#log-clear').addEventListener('click', () => { state.logLines = []; renderLogs(); });
  $$('[data-open-folder]').forEach((b) => b.addEventListener('click', () => api('/api/open-folder', { method: 'POST', body: { which: b.dataset.openFolder } }).then((r) => toast(r.path)).catch((e) => toast(e.message))));
  $('#quit-app').addEventListener('click', async () => {
    if (!confirm('Quit Kestrel and stop both models?')) return;
    try { await api('/api/quit', { method: 'POST' }); } catch { /* ignore */ }
    document.body.innerHTML = '<div style="display:flex;height:100vh;align-items:center;justify-content:center;color:#888;font-family:sans-serif">Kestrel has stopped. You can close this window.</div>';
    setTimeout(() => window.close(), 300);
  });

  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey && e.shiftKey && (e.key === 'O' || e.key === 'o')) { e.preventDefault(); newChat(); }
    if (e.key === 'Escape') { if (!$('#settings').hidden) closeSettings(); else if (state.busy) stopGeneration(); }
    if (e.ctrlKey && e.key === ',') { e.preventDefault(); openSettings(); }
  });
}

async function init() {
  try { const t = localStorage.getItem('kestrel-theme'); if (t) applyTheme(t); } catch { /* ignore */ }
  bind();
  try {
    const r = await api('/api/config');
    state.config = r.config; state.defaults = r.defaults;
    applyTheme(state.config.theme);
    setChip('#chip-thinking', state.config.thinking);
    setChip('#chip-tools', state.config.tools);
  } catch { /* ignore */ }
  await pollStatus();
  loadChats();
  renderThread();
  autoGrow();
  $('#input').focus();
  setInterval(pollStatus, 1500);
  setInterval(() => fetch('/api/ping').catch(() => {}), 5000);
  fetch('/api/ping').catch(() => {});
  setInterval(pollLogs, 1500);
  pollLogs();
}

init();
