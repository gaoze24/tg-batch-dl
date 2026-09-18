"use strict";

const $ = (id) => document.getElementById(id);

const KIND_LABEL = { channel: "频道", group: "群组", user: "私聊", bot: "机器人", self: "收藏" };
const FILTER_LABEL = { video: "视频", media: "图片和视频", photo: "图片", file: "文件" };
const STATUS_LABEL = { queued: "排队中", listing: "正在列出", downloading: "下载中", done: "完成", failed: "失败", cancelled: "已取消" };
const ACTIVE = new Set(["queued", "listing", "downloading"]);

const S = {
  tg: null,            // telegram status from /api/state
  loginTab: "qr",
  qrVersion: 0,
  chats: [],
  chat: null,          // current chat {ref,title,...}
  filter: "video",
  items: [],
  next: 0,
  total: 0,
  done: false,
  loading: false,
  token: 0,            // bumps on chat/filter change to drop stale responses
  selected: new Set(), // message ids
  lastIndex: -1,
  jobs: [],
  drawerOpen: false,
  quitting: false,
};

// ---------- helpers ----------

async function api(path, options = {}) {
  const res = await fetch(path, {
    ...options,
    headers: { "X-TGDL": "1", "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `请求失败（HTTP ${res.status}）`);
  return data;
}
const post = (path, body) => api(path, { method: "POST", body: JSON.stringify(body || {}) });

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text != null) node.textContent = text;
  return node;
}

function toast(msg, kind) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (kind ? " " + kind : "");
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, kind === "error" ? 6000 : 3000);
}

function fmtBytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return (n / 1024 ** i).toFixed(i ? 1 : 0) + " " + u[i];
}

function fmtDur(sec) {
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  const pad = (x) => String(x).padStart(2, "0");
  return h ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

function fmtDate(ts) {
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

function firstLine(s) {
  return (s || "").split("\n").map((x) => x.trim()).find(Boolean) || "";
}

function show(id, visible) { $(id).hidden = !visible; }

// ---------- app state / views ----------

async function pollState() {
  if (S.quitting) return;
  try {
    applyState(await api("/api/state"));
  } catch (e) {
    showConnecting("连不上本地程序：" + e.message + "。如果已经关闭了程序窗口，请重新打开 tgdl.exe。");
  }
  const ready = S.tg && S.tg.state === "ready";
  setTimeout(pollState, ready ? 3000 : 1000);
}

function showConnecting(text) {
  show("view-connecting", true);
  show("view-login", false);
  show("view-main", false);
  $("connecting-text").textContent = text;
  $("status").textContent = "未连接";
}

function applyState(st) {
  const t = st.telegram;
  const prev = S.tg ? S.tg.state : "";
  S.tg = t;
  const ready = t.state === "ready";
  show("btn-logout", ready);
  show("btn-links", ready);

  if (t.state === "connecting" || t.state === "offline") {
    showConnecting(t.error || "正在连接 Telegram…");
    return;
  }
  show("view-connecting", false);
  show("view-login", t.state === "login");
  show("view-main", ready);
  $("status").textContent = ready && t.self ? `已登录：${t.self.name}${t.self.username ? " (@" + t.self.username + ")" : ""}` : "未登录";

  if (t.state === "login") renderLogin(t.login);
  if (ready && prev !== "ready") loadChats(false);
}

// ---------- login ----------

function renderLogin(lv) {
  const step = lv ? lv.step : "idle";
  const method = lv ? lv.method : S.loginTab;
  if (lv) S.loginTab = method;
  document.querySelectorAll("#login-tabs .tab").forEach((b) => b.classList.toggle("active", b.dataset.tab === S.loginTab));

  const entry = step === "idle" || step === "starting" || step === "error";
  show("pane-qr", S.loginTab === "qr" && (entry || step === "qr"));
  show("pane-phone", S.loginTab === "phone" && entry);
  show("pane-code", step === "code");
  show("pane-password", step === "password");
  show("login-tabs", entry || step === "qr");

  const img = $("qr-img");
  if (step === "qr" && lv.qr_version !== S.qrVersion) {
    S.qrVersion = lv.qr_version;
    img.src = "/api/login/qr.png?v=" + encodeURIComponent(lv.qr_version);
  }
  show("qr-img", step === "qr");
  show("btn-qr", step !== "qr");
  $("btn-qr").disabled = step === "starting";
  $("btn-qr").textContent = step === "starting" ? "正在生成…" : step === "error" ? "重新生成二维码" : "生成二维码";

  $("pwd-hint").textContent = lv && lv.password_hint ? `密码提示：${lv.password_hint}` : "";
  const err = lv && lv.error ? lv.error : "";
  $("login-error").textContent = err;
  show("login-error", !!err);
  show("btn-login-restart", !!lv && step !== "starting");
}

async function loginAction(fn) {
  try {
    await fn();
    const st = await api("/api/state");
    applyState(st);
  } catch (e) {
    toast(e.message, "error");
  }
}

function bindLogin() {
  document.querySelectorAll("#login-tabs .tab").forEach((b) => b.addEventListener("click", () => {
    if (S.loginTab === b.dataset.tab) return;
    S.loginTab = b.dataset.tab;
    loginAction(() => post("/api/login/cancel"));
  }));
  $("btn-qr").addEventListener("click", () => loginAction(() => post("/api/login/qr")));
  $("btn-phone").addEventListener("click", () => loginAction(() => post("/api/login/phone", { phone: $("phone").value })));
  $("btn-code").addEventListener("click", () => loginAction(async () => {
    await post("/api/login/code", { code: $("code").value });
    $("code").value = "";
  }));
  $("btn-password").addEventListener("click", () => loginAction(async () => {
    await post("/api/login/password", { password: $("password").value });
    $("password").value = "";
  }));
  $("btn-login-restart").addEventListener("click", () => loginAction(() => post("/api/login/cancel")));
  for (const [input, button] of [["phone", "btn-phone"], ["code", "btn-code"], ["password", "btn-password"]]) {
    $(input).addEventListener("keydown", (e) => { if (e.key === "Enter") $(button).click(); });
  }
}

// ---------- chats ----------

async function loadChats(refresh) {
  $("chat-list").replaceChildren(el("li", "muted", "正在加载聊天列表…"));
  try {
    const d = await api("/api/chats" + (refresh ? "?refresh=1" : ""));
    S.chats = d.chats || [];
    renderChats();
  } catch (e) {
    $("chat-list").replaceChildren(el("li", "error", e.message));
  }
}

function renderChats() {
  const q = $("chat-search").value.trim().toLowerCase();
  const list = S.chats.filter((c) => !q || c.title.toLowerCase().includes(q) || (c.username || "").toLowerCase().includes(q));
  if (!list.length) {
    $("chat-list").replaceChildren(el("li", "muted", S.chats.length ? "没有匹配的聊天" : "聊天列表是空的"));
    return;
  }
  $("chat-list").replaceChildren(...list.map((c) => {
    const li = el("li", S.chat && S.chat.ref === c.ref ? "active" : "");
    li.append(el("span", "t", c.title), el("span", "k", KIND_LABEL[c.kind] || ""));
    li.title = c.username ? `${c.title} (@${c.username})` : c.title;
    li.addEventListener("click", () => openChat(c));
    return li;
  }));
}

function openChat(chat) {
  S.chat = chat;
  $("chat-title").textContent = chat.title;
  $("chat-title").title = chat.title;
  show("empty-chat", false);
  show("chat-view", true);
  renderChats();
  resetGrid();
}

// ---------- media grid ----------

function resetGrid() {
  S.token++;
  S.items = [];
  S.next = 0;
  S.total = 0;
  S.done = false;
  S.loading = false;
  S.selected.clear();
  S.lastIndex = -1;
  $("grid").replaceChildren();
  document.querySelectorAll("#filters button").forEach((b) => b.classList.toggle("active", b.dataset.filter === S.filter));
  updateSelbar();
  loadMore();
}

async function loadMore() {
  if (!S.chat || S.loading || S.done) return;
  S.loading = true;
  const token = S.token;
  $("grid-status").textContent = "加载中…";
  try {
    const url = `/api/chats/${encodeURIComponent(S.chat.ref)}/media?filter=${S.filter}&offset=${S.next}`;
    const page = await api(url);
    if (token !== S.token) return;
    const start = S.items.length;
    S.items.push(...page.items);
    S.total = page.total;
    S.next = page.next_offset;
    S.done = !page.next_offset;
    const grid = $("grid");
    page.items.forEach((item, i) => grid.append(card(item, start + i)));
    $("grid-status").textContent = S.items.length ? (S.done ? "已全部加载" : "") : `这个聊天里没有${FILTER_LABEL[S.filter]}`;
    updateSelbar();
  } catch (e) {
    if (token === S.token) $("grid-status").textContent = "加载失败：" + e.message;
  } finally {
    if (token === S.token) S.loading = false;
  }
  if (token === S.token && !S.done && sentinelVisible()) loadMore();
}

function sentinelVisible() {
  const r = $("sentinel").getBoundingClientRect();
  return r.top < window.innerHeight + 400;
}

function card(item, index) {
  const c = el("div", "item");
  c.dataset.index = index;
  const thumb = el("div", "thumb");
  if (item.thumb) {
    const img = new Image();
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = "";
    img.src = `/api/thumb/${encodeURIComponent(S.chat.ref)}/${item.id}`;
    img.addEventListener("error", () => img.replaceWith(el("span", "ph", item.kind === "video" ? "▶" : "▣")));
    thumb.append(img);
  } else {
    thumb.append(el("span", "ph", item.kind === "video" ? "▶" : item.kind === "photo" ? "▣" : "📄"));
  }
  if (item.kind === "video" && item.duration) thumb.append(el("span", "dur", fmtDur(item.duration)));
  thumb.append(el("span", "check"));

  const meta = el("div", "meta");
  meta.append(el("div", "name", firstLine(item.caption) || item.name));
  meta.append(el("div", "sub", `${fmtBytes(item.size)} · ${fmtDate(item.date)}`));
  c.append(thumb, meta);
  c.title = item.caption ? `${item.caption}\n\n${item.name}` : item.name;
  c.classList.toggle("selected", S.selected.has(item.id));
  c.addEventListener("click", (e) => toggle(index, e.shiftKey));
  return c;
}

function toggle(index, range) {
  const item = S.items[index];
  const on = !S.selected.has(item.id);
  if (range && S.lastIndex >= 0) {
    const [a, b] = S.lastIndex < index ? [S.lastIndex, index] : [index, S.lastIndex];
    for (let i = a; i <= b; i++) setSelected(i, on);
  } else {
    setSelected(index, on);
  }
  S.lastIndex = index;
  updateSelbar();
}

function setSelected(index, on) {
  const id = S.items[index].id;
  if (on) S.selected.add(id); else S.selected.delete(id);
  const node = $("grid").children[index];
  if (node) node.classList.toggle("selected", on);
}

function updateSelbar() {
  const bytes = S.items.filter((i) => S.selected.has(i.id)).reduce((sum, i) => sum + i.size, 0);
  const loaded = S.total && S.total !== S.items.length ? `已加载 ${S.items.length} / 共 ${S.total} 个` : `共 ${S.items.length} 个`;
  $("sel-info").textContent = S.selected.size ? `${loaded} · 已选 ${S.selected.size} 个（${fmtBytes(bytes)}）` : loaded;
  $("btn-download").disabled = S.selected.size === 0;
  $("btn-download").textContent = S.selected.size ? `下载所选（${S.selected.size}）` : "下载所选";
  $("btn-all").textContent = `下载全部${FILTER_LABEL[S.filter]}`;
}

async function downloadSelected() {
  if (!S.selected.size) return;
  try {
    await post("/api/jobs", { ref: S.chat.ref, ids: [...S.selected] });
    toast(`已加入下载：${S.selected.size} 个文件`);
    S.selected.clear();
    document.querySelectorAll("#grid .item.selected").forEach((n) => n.classList.remove("selected"));
    updateSelbar();
    openDrawer();
  } catch (e) {
    toast(e.message, "error");
  }
}

async function downloadAll() {
  const what = FILTER_LABEL[S.filter];
  const count = S.total ? `全部 ${S.total} 个` : "全部";
  if (!confirm(`下载「${S.chat.title}」里${count}${what}？\n已经下载过的文件会自动跳过。`)) return;
  try {
    await post("/api/jobs", { ref: S.chat.ref, all: true, filter: S.filter });
    toast("已加入下载");
    openDrawer();
  } catch (e) {
    toast(e.message, "error");
  }
}

function bindMain() {
  $("chat-search").addEventListener("input", renderChats);
  $("btn-refresh-chats").addEventListener("click", () => loadChats(true));
  document.querySelectorAll("#filters button").forEach((b) => b.addEventListener("click", () => {
    if (S.filter === b.dataset.filter) return;
    S.filter = b.dataset.filter;
    resetGrid();
  }));
  $("btn-select-all").addEventListener("click", () => {
    S.items.forEach((_, i) => setSelected(i, true));
    updateSelbar();
  });
  $("btn-select-none").addEventListener("click", () => {
    S.items.forEach((_, i) => setSelected(i, false));
    S.lastIndex = -1;
    updateSelbar();
  });
  $("btn-download").addEventListener("click", downloadSelected);
  $("btn-all").addEventListener("click", downloadAll);
  new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) loadMore();
  }, { rootMargin: "400px" }).observe($("sentinel"));
}

// ---------- jobs ----------

async function pollJobs() {
  if (S.quitting) return;
  try {
    const d = await api("/api/jobs");
    S.jobs = d.jobs || [];
    renderJobs();
  } catch (_) { /* state poll reports connectivity */ }
  setTimeout(pollJobs, S.drawerOpen ? 1500 : 4000);
}

function renderJobs() {
  const active = S.jobs.filter((j) => ACTIVE.has(j.status)).length;
  $("jobs-badge").textContent = active;
  show("jobs-badge", active > 0);
  if (!S.drawerOpen) return;
  const list = $("job-list");
  if (!S.jobs.length) {
    list.replaceChildren(el("p", "muted", "还没有下载任务。在聊天里选中文件后点「下载所选」。"));
    return;
  }
  const open = new Set([...list.querySelectorAll("details[open]")].map((d) => d.dataset.key));
  list.replaceChildren(...S.jobs.map((j) => jobNode(j, open)));
}

function jobNode(j, openDetails) {
  const node = el("div", "job");
  const top = el("div", "job-top");
  top.append(el("span", "job-title", j.title), el("span", `pill s-${j.status}`, STATUS_LABEL[j.status] || j.status));
  node.append(top);

  const processed = j.done + j.existing + j.failed;
  const bar = el("div", "bar");
  const fill = el("div");
  fill.style.width = (j.status === "done" ? 100 : j.total ? Math.min(100, (processed / j.total) * 100) : 0) + "%";
  bar.append(fill);
  node.append(bar);

  const facts = [];
  if (j.total) facts.push(`${processed} / ${j.total} 个文件`);
  if (j.existing) facts.push(`跳过已存在 ${j.existing}`);
  if (j.failed) facts.push(`失败 ${j.failed}`);
  if (j.bytes_done) facts.push(`已下载 ${fmtBytes(j.bytes_done)}`);
  if (j.status === "downloading" && j.speed > 1) facts.push(`${fmtBytes(j.speed)}/s`);
  node.append(el("div", "job-facts", facts.join(" · ")));
  if (j.message) node.append(el("div", "job-facts", j.message));

  if (j.active.length) {
    const box = el("div", "active-files");
    for (const f of j.active) {
      box.append(el("div", "n", `${f.name}  ${fmtBytes(f.done)} / ${fmtBytes(f.size)}`));
      const b = el("div", "bar thin");
      const bf = el("div");
      bf.style.width = (f.size ? Math.min(100, (f.done / f.size) * 100) : 0) + "%";
      b.append(bf);
      box.append(b);
    }
    node.append(box);
  }

  if (j.errors.length) {
    const d = el("details");
    d.dataset.key = j.id;
    d.open = openDetails.has(j.id);
    d.append(el("summary", "", `失败详情（${j.errors.length}）`), el("pre", "", j.errors.join("\n")));
    node.append(d);
  }

  const actions = el("div", "job-actions");
  const btn = (text, fn) => {
    const b = el("button", "btn", text);
    b.addEventListener("click", fn);
    actions.append(b);
  };
  if (ACTIVE.has(j.status)) btn("取消", () => jobAction(`/api/jobs/${j.id}/cancel`, "已取消"));
  if (!ACTIVE.has(j.status) && j.failed) btn("重试失败的", () => jobAction(`/api/jobs/${j.id}/retry`, "已重新加入下载"));
  btn("打开文件夹", () => jobAction(`/api/jobs/${j.id}/open`));
  node.append(actions);
  return node;
}

async function jobAction(path, okText) {
  try {
    await post(path);
    if (okText) toast(okText);
    const d = await api("/api/jobs");
    S.jobs = d.jobs || [];
    renderJobs();
  } catch (e) {
    toast(e.message, "error");
  }
}

function openDrawer() {
  S.drawerOpen = true;
  show("drawer", true);
  renderJobs();
}

function bindJobs() {
  $("btn-jobs").addEventListener("click", () => (S.drawerOpen ? closeDrawer() : openDrawer()));
  $("btn-close-drawer").addEventListener("click", closeDrawer);
  $("btn-clear").addEventListener("click", () => jobAction("/api/jobs/clear"));
  $("btn-open-root").addEventListener("click", () => jobAction("/api/open-dir"));
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && S.drawerOpen) closeDrawer(); });
}

function closeDrawer() {
  S.drawerOpen = false;
  show("drawer", false);
}

// ---------- settings, links, app ----------

let settingsCache = null;

async function openSettings() {
  try {
    settingsCache = await api("/api/settings");
  } catch (e) {
    toast(e.message, "error");
    return;
  }
  $("set-dir").value = settingsCache.download_dir;
  $("set-threads").value = settingsCache.threads;
  $("set-limit").value = settingsCache.limit;
  $("set-proxy").value = settingsCache.proxy || "";
  $("set-port").value = settingsCache.port;
  $("set-api-id").value = settingsCache.api_id || "";
  $("set-api-hash").value = settingsCache.api_hash || "";
  show("set-error", false);
  $("dlg-settings").showModal();
}

async function saveSettings() {
  const body = {
    port: parseInt($("set-port").value, 10) || 0,
    download_dir: $("set-dir").value.trim(),
    threads: parseInt($("set-threads").value, 10) || 0,
    limit: parseInt($("set-limit").value, 10) || 0,
    proxy: $("set-proxy").value.trim(),
    api_id: parseInt($("set-api-id").value, 10) || 0,
    api_hash: $("set-api-hash").value.trim(),
  };
  try {
    await post("/api/settings", body);
    const restart = body.port !== settingsCache.port || body.proxy !== (settingsCache.proxy || "") ||
      body.api_id !== (settingsCache.api_id || 0) || body.api_hash !== (settingsCache.api_hash || "");
    $("dlg-settings").close();
    toast(restart ? "已保存，重启程序后生效" : "已保存");
  } catch (e) {
    $("set-error").textContent = e.message;
    show("set-error", true);
  }
}

async function submitLinks() {
  const links = $("links-text").value.split(/\s+/).map((s) => s.trim()).filter(Boolean);
  if (!links.length) return;
  $("links-submit").disabled = true;
  try {
    const d = await post("/api/jobs", { links });
    $("dlg-links").close();
    $("links-text").value = "";
    toast(`已加入下载：${d.jobs.length} 个任务`);
    openDrawer();
  } catch (e) {
    $("links-error").textContent = e.message;
    show("links-error", true);
  } finally {
    $("links-submit").disabled = false;
  }
}

function bindApp() {
  $("btn-settings").addEventListener("click", openSettings);
  $("set-cancel").addEventListener("click", () => $("dlg-settings").close());
  $("set-save").addEventListener("click", saveSettings);

  $("btn-links").addEventListener("click", () => {
    show("links-error", false);
    $("dlg-links").showModal();
  });
  $("links-cancel").addEventListener("click", () => $("dlg-links").close());
  $("links-submit").addEventListener("click", submitLinks);

  $("btn-logout").addEventListener("click", async () => {
    if (!confirm("退出登录？本机保存的登录凭证会被删除，下次需要重新扫码。")) return;
    try {
      await post("/api/logout");
      S.chat = null;
      S.chats = [];
      show("chat-view", false);
      show("empty-chat", true);
    } catch (e) {
      toast(e.message, "error");
    }
  });

  $("btn-quit").addEventListener("click", async () => {
    const running = S.jobs.some((j) => ACTIVE.has(j.status));
    if (!confirm(running ? "还有任务在下载，确定退出程序吗？下次重新提交会跳过已下完的文件。" : "退出程序？")) return;
    try { await post("/api/quit"); } catch (_) { /* already gone */ }
    S.quitting = true;
    document.body.replaceChildren(el("p", "placeholder", "程序已退出，可以关闭这个页面了。"));
  });
}

bindLogin();
bindMain();
bindJobs();
bindApp();
pollState();
pollJobs();
