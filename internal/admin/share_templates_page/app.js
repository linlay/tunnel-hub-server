"use strict";
const el = (id) => document.getElementById(id);
const endpoint = "/api/admin/share-templates";
let versions = [];
let loadSequence = 0;
let detailSequence = 0;
let activeDays = "30";
let uploadEnabled = false;
let maxUploadBytes = 0;
let writing = false;

function message(text, error = false) {
  el("message").textContent = text;
  el("message").className = error ? "error" : "muted";
}
function showLogin() {
  loadSequence++; uploadEnabled = false; el("upload").disabled = true; el("refresh").disabled = false;
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
async function request(path, options = {}, timeout = 15000) {
  let response;
  try { response = await fetch(path, { ...options, credentials: "same-origin", cache: "no-store", signal: AbortSignal.timeout(timeout) }); }
  catch (error) { throw new Error(options.headers?.["X-Template-Action"] ? "操作结果待确认，请刷新查看；不会自动重试。" : "连接失败，请刷新重试。"); }
  if (response.status === 401) {
    const error = new Error(path === "/api/admin/login" ? "账号或密码无效。" : "请登录管理员账号。已有会话可能已过期。");
    error.status = 401;
    throw error;
  }
  if (!response.ok) {
    const messages = { 400: "模板包或请求无效。", 403: "权限或同源校验未通过。", 404: "版本不存在，请刷新。", 409: "已有版本损坏或冲突，请选择其他有效版本。", 413: "模板包超过大小限制。", 415: "请选择 ZIP 模板包。", 503: "模板存储暂不可用。" };
    throw new Error(messages[response.status] || "操作失败（" + response.status + "）。");
  }
  try { return await response.json(); }
  catch (error) { throw new Error(options.headers?.["X-Template-Action"] ? "操作响应未完整收到，结果待确认，请刷新查看。" : "响应无效，请刷新重试。"); }
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
    const badge = document.createElement("span"); badge.className = "badge" + (item.error ? " error" : item.current ? " current" : ""); badge.textContent = item.current ? (item.error ? "当前不可用" : "当前启用") : (item.error ? "不可用" : item.canActivate ? "可启用" : "仅历史资源");
    version.append(hash, badge);
    if (item.error) { const detail = document.createElement("p"); detail.className = "error"; detail.textContent = item.error; version.append(detail); }
    const source = cell(row, "", "source");
    const commit = document.createElement("code"); commit.textContent = item.build?.commit ? item.build.commit.slice(0, 12) : "未记录"; commit.title = item.build?.commit || "未记录";
    const full = document.createElement("details"); const label = document.createElement("summary"); label.textContent = "完整 commit"; full.append(label, document.createTextNode(item.build?.commit || "未记录"));
    source.append(commit, document.createElement("br"), document.createTextNode(item.build?.dirty === true ? "含未提交改动" : item.build?.dirty === false ? "工作区干净" : "工作区状态未知"), full);
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
    button.addEventListener("click", () => showDetail(item)); const actions = cell(row, ""); actions.append(button);
    if (item.canActivate && (!item.current || item.error)) { const activate = document.createElement("button"); activate.textContent = "启用"; activate.disabled = writing; activate.addEventListener("click", () => writeTemplate(item.assetSet)); actions.append(activate); }
    el("versions").append(row);
  }
  if (!items.length) { const row = document.createElement("tr"); cell(row, "没有符合筛选条件的版本。").colSpan = 10; el("versions").append(row); }
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
    uploadEnabled = data.uploadEnabled; maxUploadBytes = data.maxUploadBytes;
    el("upload").disabled = writing || !uploadEnabled;
    el("upload-limit").textContent = "最大 " + Math.round(maxUploadBytes / 1024 / 1024) + " MiB；" + (uploadEnabled ? "上传后需手动启用。" : "上传不可用，请检查模板存储权限。");
    el("template-status").textContent = (data.currentAssetSet ? "当前选择：" + data.currentAssetSet : "当前选择不可用") + (data.error ? " · " + data.error : "");
    el("template-status").className = data.error ? "error" : "muted";
    el("login-panel").hidden = true; el("dashboard").hidden = false; el("logout").hidden = false;
    el("window").textContent = "统计区间：" + data.startDay + " 至 " + data.endDay + "（UTC）。统计起点晚于区间开始的版本，仅覆盖实际采集时间。";
    render(); message("已更新 · " + new Date().toLocaleTimeString("zh-CN"));
    return true;
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
async function writeTemplate(assetSet) {
  if (writing) return;
  const file = el("template-file").files[0];
  if (!assetSet && (!file || !/\.zip$/i.test(file.name) || file.size > maxUploadBytes)) { message("请选择限制大小内的 ZIP 文件。", true); return; }
  writing = true; loadSequence++; el("upload").disabled = true; el("logout").disabled = true; render(); message(assetSet ? "正在启用…" : "正在上传…");
  try {
    const result = await request(endpoint + (assetSet ? "/" + encodeURIComponent(assetSet) + "/activate" : "/upload"), { method: "POST", headers: { "X-Template-Action": "1", ...(!assetSet ? { "Content-Type": "application/zip" } : {}) }, ...(!assetSet ? { body: file } : {}) }, 120000);
    const refreshed = await load();
    if (!assetSet) el("template-file").value = "";
    if (refreshed) message(assetSet ? "已启用。" : result.alreadyExists ? "版本已存在，沿用已有来源。" : "已上传，尚未启用。");
  } catch (error) { if (error.status === 401) showLogin(); message(error.message, true); }
  finally { writing = false; el("upload").disabled = !uploadEnabled; el("logout").disabled = false; render(); }
}
el("upload").addEventListener("click", () => writeTemplate());
el("filter").addEventListener("change", render);
el("days").addEventListener("change", load);
el("refresh").addEventListener("click", load);
el("close-detail").addEventListener("click", () => { detailSequence++; el("detail").close(); });
load();
