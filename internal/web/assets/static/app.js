/* TG 桥 — chat frontend (no framework). All user text is inserted via
 * textContent; nothing from the API is ever written as HTML. */
"use strict";

const $ = (sel) => document.querySelector(sel);
const app = $("#app");
const listEl = $("#chat-list");
const msgsEl = $("#msgs");
const inputEl = $("#input");

let chats = [];
let current = null;
let oldest = 0;
let total = 0;
let tempSeq = 0;
let currentReadOutbox = 0; // peer's read watermark for the open chat
const seen = new Set(); // message row ids already rendered
const localUnread = {}; // in-session badge increments, calibrated by /api/chats

/* ---------- helpers ---------- */

async function api(path, opts = {}) {
  const res = await fetch(path, { ...opts, headers: { "Content-Type": "application/json", ...(opts.headers || {}) } });
  if (res.status === 401) {
    location.href = "/login";
    throw new Error("unauthorized");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
  return data;
}

function fmtTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const now = new Date();
  const hm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  if (d.toDateString() === now.toDateString()) return hm;
  return (d.getMonth() + 1) + "-" + String(d.getDate()).padStart(2, "0") + " " + hm;
}

function chatName(c) {
  return c.title || c.username || ("ID " + c.tg_user_id);
}

let toastTimer = null;
function toast(text) {
  const t = $("#toast");
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 3800);
}

/* ---------- unread (server watermark + in-session increments) ---------- */

function unreadFor(c) {
  return (c.unread || 0) + (localUnread[c.id] || 0);
}

/* ---------- chat list ---------- */

async function loadChats() {
  chats = await api("/api/chats");
  renderList();
}

// Deterministic pastel hue per peer so avatars stay stable across renders.
function hueOf(str) {
  let h = 0;
  for (let i = 0; i < str.length; i++) h = (h * 31 + str.charCodeAt(i)) >>> 0;
  return h % 360;
}

function renderList() {
  listEl.textContent = "";
  $("#chat-empty").hidden = chats.length > 0;
  for (const c of chats) {
    const li = document.createElement("li");
    li.className = "chat-item" +
      (current && current.id === c.id ? " active" : "") +
      (!c.enabled ? " disabled-trans" : "");
    li.dataset.id = c.id;

    const av = document.createElement("span");
    av.className = "avatar";
    av.style.background = `hsl(${hueOf(String(c.tg_user_id))} 48% 42%)`;
    av.textContent = (chatName(c)[0] || "?").toUpperCase();

    const meta = document.createElement("div");
    meta.className = "chat-meta";
    const row1 = document.createElement("div");
    row1.className = "chat-row1";
    const name = document.createElement("span");
    name.className = "chat-name";
    name.textContent = chatName(c);
    const right = document.createElement("span");
    right.className = "chat-right";
    const time = document.createElement("span");
    time.className = "chat-time";
    time.textContent = fmtTime(c.last_time);
    right.append(time);
    const n = unreadFor(c);
    if (n > 0) {
      const badge = document.createElement("span");
      badge.className = "badge";
      badge.textContent = n > 99 ? "99+" : String(n);
      right.append(badge);
    }
    row1.append(name, right);
    const snip = document.createElement("div");
    snip.className = "chat-snippet";
    snip.textContent = c.last_snippet || (!c.enabled ? "未启用翻译" : "");
    meta.append(row1, snip);

    // Switch: label natively forwards clicks to the checkbox. No manual
    // click handlers — the previous slider handler double-toggled it,
    // and the label click must not bubble into openChat.
    const sw = document.createElement("label");
    sw.className = "switch";
    sw.title = "启用翻译";
    sw.addEventListener("click", (ev) => ev.stopPropagation());
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.checked = c.enabled;
    cb.addEventListener("change", async () => {
      const on = cb.checked;
      li.classList.toggle("disabled-trans", !on);
      try {
        await api(`/api/chats/${c.id}/enable`, { method: "POST", body: JSON.stringify({ enabled: on }) });
        c.enabled = on;
      } catch (e) {
        cb.checked = !on;
        li.classList.toggle("disabled-trans", !on);
        toast("切换失败: " + e.message);
      }
    });
    const slider = document.createElement("span");
    slider.className = "slider";
    sw.append(cb, slider);

    li.append(av, meta, sw);
    li.addEventListener("click", () => openChat(c.id));
    listEl.appendChild(li);
  }
}

/* ---------- messages ---------- */

function nearBottom() {
  return msgsEl.scrollHeight - msgsEl.scrollTop - msgsEl.clientHeight < 120;
}
function scrollBottom() {
  msgsEl.scrollTop = msgsEl.scrollHeight;
}

function renderMsg(m) {
  const div = document.createElement("div");
  div.className = "msg " + (m.direction === "out" ? "out" : "in");
  div.dataset.id = m.id;

  const main = document.createElement("div");
  main.className = "main";
  const sub = document.createElement("div");

  if (m.direction === "out") {
    main.textContent = m.source_text;
    if (m.status === "done" || m.status === "skipped") {
      sub.className = "sub";
      sub.textContent = m.target_text;
    } else if (m.status === "failed") {
      sub.className = "note";
      sub.textContent = "发送失败,内容未送达";
    } else {
      sub.className = "pending";
      sub.textContent = "翻译中…";
    }
  } else {
    if (m.status === "done") {
      main.textContent = m.target_text;
      sub.className = "sub";
      sub.textContent = m.source_text;
    } else if (m.status === "skipped") {
      main.textContent = m.source_text;
    } else if (m.status === "failed") {
      main.textContent = m.source_text;
      sub.className = "note";
      sub.textContent = "翻译失败";
    } else {
      main.textContent = m.source_text;
      sub.className = "pending";
      sub.textContent = "翻译中…";
    }
  }

  div.append(main);
  if (sub.textContent) div.append(sub);
  const t = document.createElement("div");
  t.className = "time";
  // Read ticks on outgoing messages, from the peer's outbox watermark.
  if (m.direction === "out" && m.status !== "pending" && m.status !== "failed" && m.tg_msg_id > 0) {
    const tick = document.createElement("span");
    tick.className = "ticks";
    tick.innerHTML = ticksSVG(m.tg_msg_id <= currentReadOutbox);
    t.append(tick);
  }
  t.append(document.createTextNode(fmtTime(m.created_at)));
  div.append(t);
  return div;
}

function appendMsg(m, force = false) {
  if (seen.has(m.id) && !force) return;
  seen.add(m.id);
  const stick = nearBottom();
  msgsEl.appendChild(renderMsg(m));
  if (stick) scrollBottom();
}

// replaceTemp settles an optimistic bubble once the POST responds.
// The SSE stream may have already rendered the real message (or even
// replaced the temp node itself) — every path converges to exactly one
// bubble keyed by the row id.
function replaceTemp(tempId, m) {
  const node = msgsEl.querySelector(`[data-id="${tempId}"]`);
  const already = seen.has(m.id);
  seen.delete(tempId);
  seen.add(m.id);
  if (node && already) {
    node.remove();
  } else if (node) {
    node.replaceWith(renderMsg(m));
    scrollBottom();
  } else if (!already) {
    appendMsg(m);
  }
}

// ticksSVG draws Telegram-style check marks: one stroke for sent, a
// second offset stroke once the peer has read the message.
function ticksSVG(read) {
  const stroke = "fill=\"none\" stroke=\"currentColor\" stroke-width=\"1.8\" stroke-linecap=\"round\" stroke-linejoin=\"round\"";
  const first = `<path d="M1 6.5 L4.5 10 L10.5 3.5" ${stroke}/>`;
  const second = `<path d="M7.5 6.5 L11 10 L17 3.5" ${stroke}/>`;
  return `<svg class="tick" width="18" height="13" viewBox="0 0 18 13" aria-hidden="true">${first}${read ? second : ""}</svg>`;
}

async function loadMessages(before) {
  if (!current) return;
  const data = await api(`/api/chats/${current.id}/messages?before=${before}&limit=50`);
  total = data.total;
  const list = data.messages;
  if (before) {
    for (let i = list.length - 1; i >= 0; i--) {
      const m = list[i];
      if (!seen.has(m.id)) {
        seen.add(m.id);
        msgsEl.prepend(renderMsg(m));
      }
      oldest = Math.min(oldest || Infinity, m.id);
    }
  } else {
    msgsEl.textContent = "";
    seen.clear();
    for (const m of list) appendMsg(m);
    oldest = list.length ? list[0].id : 0;
  }
  $("#load-more").hidden = !(oldest > 1 && total > 0 && list.length >= 50);
  scrollBottom();
}

function setDrawer(open) {
  app.classList.toggle("list-open", open);
  $("#drawer-backdrop").hidden = !open;
}

async function openChat(id) {
  current = chats.find((c) => c.id === id) || null;
  if (!current) return;
  setDrawer(false);
  $("#empty-hint").hidden = true;
  $("#peer-name").textContent = chatName(current);
  $("#peer-sub").textContent = current.username ? "@" + current.username : "";
  const pa = $("#peer-avatar");
  pa.style.background = `hsl(${hueOf(String(current.tg_user_id))} 48% 42%)`;
  pa.textContent = (chatName(current)[0] || "?").toUpperCase();
  oldest = 0;

  // Advance the read watermark server-side, push the receipt to
  // Telegram, and learn the peer's outbox watermark for the ticks.
  const read = await api(`/api/chats/${id}/read`, { method: "POST" }).catch(() => null);
  if (read && typeof read.read_outbox === "number") {
    currentReadOutbox = read.read_outbox;
  }
  if (localUnread[id]) {
    delete localUnread[id];
    renderList();
  }
  await loadMessages(0);
  renderList();
  loadChats().catch(() => {}); // recalibrate the server-side badge
  inputEl.focus();
}

/* ---------- sending ---------- */

async function sendMsg() {
  if (!current) { toast("先在左侧选择一个对话"); setDrawer(true); return; }
  if (!current.enabled) { toast("该对话未启用翻译"); setDrawer(true); return; }
  const text = inputEl.value.trim();
  if (!text) return;

  const tempId = "tmp-" + (++tempSeq);
  appendMsg({ id: tempId, direction: "out", source_text: text, target_text: "", status: "pending", created_at: new Date().toISOString() });
  inputEl.value = "";
  autoGrow();
  const sendBtn = $("#send");
  sendBtn.disabled = true;

  try {
    const r = await api(`/api/chats/${current.id}/send`, { method: "POST", body: JSON.stringify({ text }) });
    replaceTemp(tempId, { id: r.row_id, direction: "out", source_text: text, target_text: r.target, status: "done", created_at: new Date().toISOString() });
  } catch (e) {
    replaceTemp(tempId, { id: "fail-" + tempSeq, direction: "out", source_text: text, target_text: "", status: "failed", created_at: new Date().toISOString() });
    toast("发送失败: " + e.message);
    if (!inputEl.value) { inputEl.value = text; autoGrow(); }
  } finally {
    sendBtn.disabled = false;
    inputEl.focus();
  }
}

function autoGrow() {
  inputEl.style.height = "auto";
  inputEl.style.height = Math.min(inputEl.scrollHeight, window.innerHeight * 0.38) + "px";
}

/* ---------- SSE ---------- */

let chatsRefreshTimer = null;
function connectStream() {
  const es = new EventSource("/api/stream");
  es.onmessage = (ev) => {
    let data;
    try { data = JSON.parse(ev.data); } catch { return; }
    if (data.type === "message") {
      if (current && data.chat_id === current.id) {
        if (String(data.message.id).startsWith("tmp-")) return;
        const node = msgsEl.querySelector(`[data-id="${data.message.id}"]`);
        if (node) {
          node.replaceWith(renderMsg(data.message));
        } else {
          appendMsg(data.message);
        }
      } else {
        localUnread[data.chat_id] = (localUnread[data.chat_id] || 0) + 1;
        renderList();
      }
    } else if (data.type === "chats") {
      clearTimeout(chatsRefreshTimer);
      chatsRefreshTimer = setTimeout(loadChats, 300);
    }
  };
}

/* ---------- TG auth wizard ---------- */

async function submitAuth(action, fields, btn) {
  const errEl = $("#w-err");
  errEl.hidden = true;
  btn.disabled = true;
  try {
    const res = await api("/api/auth/" + action, { method: "POST", body: JSON.stringify(fields) });
    if (res.error) {
      errEl.textContent = res.error;
      errEl.hidden = false;
    }
    if (res.status === "ok") {
      location.reload();
      return;
    }
    switchWizardView(res.status);
  } catch (e) {
    errEl.textContent = "请求失败: " + e.message;
    errEl.hidden = false;
  } finally {
    btn.disabled = false;
  }
}

function switchWizardView(state) {
  for (const v of ["#w-phone", "#w-code", "#w-pass"]) $(v).hidden = true;
  const map = { need_phone: "#w-phone", need_code: "#w-code", need_password: "#w-pass" };
  const sel = map[state];
  if (sel) {
    $(sel).hidden = false;
    $(sel).querySelector("input").focus();
  }
}

async function boot() {
  const st = await api("/api/auth/state");
  if (st.status !== "ok") {
    $("#auth-wizard").hidden = false;
    switchWizardView(st.status);
    return;
  }
  // On phones the chat pane starts empty; lead with the list instead.
  if (window.matchMedia("(max-width: 768px)").matches) {
    setDrawer(true);
  }
  loadChats().catch((e) => toast("加载对话失败: " + e.message));
  connectStream();
}

$("#w-phone-btn").addEventListener("click", () =>
  submitAuth("code", { phone: $("#w-phone-input").value.trim() }, $("#w-phone-btn")));
$("#w-code-btn").addEventListener("click", () =>
  submitAuth("verify", { code: $("#w-code-input").value.trim() }, $("#w-code-btn")));
$("#w-code-resend").addEventListener("click", () =>
  submitAuth("code", { phone: null }, $("#w-code-btn")));
$("#w-pass-btn").addEventListener("click", () =>
  submitAuth("password", { password: $("#w-pass-input").value }, $("#w-pass-btn")));
for (const [sel, fn] of [
  ["#w-phone-input", (e) => { if (e.key === "Enter") $("#w-phone-btn").click(); }],
  ["#w-code-input", (e) => { if (e.key === "Enter") $("#w-code-btn").click(); }],
  ["#w-pass-input", (e) => { if (e.key === "Enter") $("#w-pass-btn").click(); }],
]) {
  $(sel).addEventListener("keydown", fn);
}

/* ---------- bindings ---------- */

$("#composer").addEventListener("submit", (ev) => { ev.preventDefault(); sendMsg(); });
inputEl.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
    ev.preventDefault();
    sendMsg();
  }
});
inputEl.addEventListener("input", autoGrow);
$("#back").addEventListener("click", () => setDrawer(true));
$("#drawer-backdrop").addEventListener("click", () => setDrawer(false));
$("#load-more").addEventListener("click", () => loadMessages(oldest));

boot();
