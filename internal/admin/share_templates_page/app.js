"use strict";
const el = (id) => document.getElementById(id);
const endpoint = "/api/admin/share-templates";
let versions = [];
let loadSequence = 0;
let detailSequence = 0;
let activeDays = "30";

function message(text, error = false) {
  el("message").textContent = text;
  el("message").className = error ? "error" : "muted";
}
function showLogin() {
  versions = [];
  el("versions").replaceChildren();
  el("summary").textContent = "";
  el("window").textContent = "";
  el("detail-rows").replaceChildren();
  el("detail-version").textContent = "";
  el("detail-message").textContent = "";
  el("detail").close();
  detailSequence++;
  el("dashboard").hidden = true;
  el("logout").hidden = true;
  el("login-panel").hidden = false;
}
async function request(path, options = {}) {
  const response = await fetch(path, { ...options, credentials: "same-origin", cache: "no-store", signal: AbortSignal.timeout(15000) });
  if (response.status === 401) {
    const error = new Error(options.method === "POST" ? "账号或密码无效。" : "请登录管理员账号。已有会话可能已过期。");
    error.status = 401;
    throw error;
  }
  if (!response.ok) throw new Error(response.status === 403 ? "当前账号没有管理员权限。" : "读取失败，请稍后重试（" + response.status + "）。");
  return response.json();
}
function time(value) {
  return value ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "—";
}
function cell(row, text, className) {
  const node = document.createElement("td");
  node.textContent = text;
  if (className) node.className = className;
  row.append(node);
  return node;
}
function used(item) { return item.usage && (item.usage.renderCount > 0 || item.usage.resourceRequestCount > 0); }
function render() {
  const filter = el("filter").value;
  const items = versions.filter((item) => filter === "all" || (filter === "current" && item.current) ||
    (filter === "used" && used(item)) || (filter === "idle" && item.usage && !used(item)) || (filter === "unknown" && !item.usage));
  el("versions").replaceChildren();
  el("summary").textContent = items.length + " / " + versions.length + " 个版本";
  for (const item of items) {
    const row = document.createElement("tr");
    const version = cell(row, "");
    const hash = document.createElement("code"); hash.className = "version"; hash.textContent = item.assetSet;
    const badge = document.createElement("span"); badge.className = "badge" + (item.current ? " current" : ""); badge.textContent = item.current ? "当前启用" : "历史保留";
    version.append(hash, badge);
    const usage = item.usage;
    cell(row, !usage ? "统计未知" : used(item) ? "有使用记录" : "未观察到使用");
    cell(row, usage ? usage.renderCount.toLocaleString() : "—");
    cell(row, usage ? usage.shareCount.toLocaleString() : "—");
    cell(row, usage ? usage.resourceRequestCount.toLocaleString() : "—");
    const latest = usage && [usage.lastRenderedAt, usage.lastResourceRequestedAt].filter(Boolean).sort().pop();
    cell(row, time(latest), "date");
    cell(row, (item.sizeBytes / 1024 / 1024).toFixed(1) + " MiB · " + item.fileCount + " 文件");
    cell(row, usage ? time(usage.observedSince) : "未知", "date");
    const button = document.createElement("button"); button.textContent = "查看分享";
    button.addEventListener("click", () => showDetail(item)); cell(row, "").append(button);
    el("versions").append(row);
  }
  if (!items.length) { const row = document.createElement("tr"); cell(row, "没有符合筛选条件的版本。").colSpan = 9; el("versions").append(row); }
}
async function load() {
  const sequence = ++loadSequence;
  const days = el("days").value;
  el("refresh").disabled = true;
  el("detail").close(); detailSequence++;
  message("正在读取版本与使用情况…");
  try {
    const data = await request(endpoint + "?days=" + days);
    if (sequence !== loadSequence) return;
    versions = data.items; activeDays = days;
    el("login-panel").hidden = true; el("dashboard").hidden = false; el("logout").hidden = false;
    el("window").textContent = "统计区间：" + data.startDay + " 至 " + data.endDay + "（UTC）。统计起点晚于区间开始的版本，仅覆盖实际采集时间。";
    render(); message("已更新 · " + new Date().toLocaleTimeString("zh-CN"));
  } catch (error) {
    if (sequence === loadSequence) {
      versions = []; el("versions").replaceChildren(); el("summary").textContent = ""; el("window").textContent = "";
      if (error.status === 401) showLogin();
      message(error.message, true);
    }
  } finally { if (sequence === loadSequence) el("refresh").disabled = false; }
}
async function showDetail(item) {
  const sequence = ++detailSequence;
  el("detail-version").textContent = item.assetSet;
  el("detail-rows").replaceChildren(); el("detail-message").textContent = "正在读取…";
  el("detail").showModal();
  try {
    const data = await request(endpoint + "/" + encodeURIComponent(item.assetSet) + "/shares?days=" + activeDays);
    if (sequence !== detailSequence) return;
    el("detail-message").textContent = data.items.length ? "显示 " + data.items.length + " 条记录" : "所选时间内没有记录到页面渲染。";
    for (const item of data.items) {
      const row = document.createElement("tr"); const title = cell(row, item.title);
      const id = document.createElement("code"); id.className = "version"; id.textContent = item.id; title.append(document.createElement("br"), id);
      cell(row, item.renderCount.toLocaleString()); cell(row, time(item.lastRenderedAt), "date"); el("detail-rows").append(row);
    }
  } catch (error) {
    if (sequence === detailSequence) {
      if (error.status === 401) { showLogin(); message(error.message, true); }
      else el("detail-message").textContent = error.message;
    }
  }
}
el("login-form").addEventListener("submit", async (event) => {
  event.preventDefault(); const form = event.currentTarget; const button = form.querySelector("button"); button.disabled = true;
  try {
    await request("/api/admin/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: form.elements.username.value, password: form.elements.password.value }) });
    await load();
  } catch (error) { message(error.message, true); }
  finally { form.elements.password.value = ""; button.disabled = false; }
});
el("logout").addEventListener("click", async () => {
  el("logout").disabled = true; loadSequence++;
  try { await request("/api/admin/logout", { method: "POST" }); showLogin(); message("已退出登录。"); }
  catch (error) { if (error.status === 401) showLogin(); message(error.message, true); }
  finally { el("logout").disabled = false; el("refresh").disabled = false; }
});
el("filter").addEventListener("change", render);
el("days").addEventListener("change", load);
el("refresh").addEventListener("click", load);
el("close-detail").addEventListener("click", () => { detailSequence++; el("detail").close(); });
load();
