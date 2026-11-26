/**
 * popup.js —— 配置服务端地址 / token，展示最近一次上报结果。
 */
"use strict";

var els = {
  serverUrl: document.getElementById("serverUrl"),
  token: document.getElementById("token"),
  save: document.getElementById("save"),
  test: document.getElementById("test"),
  status: document.getElementById("status"),
  resultAt: document.getElementById("resultAt"),
  resultBody: document.getElementById("resultBody")
};

function setStatus(text, isError) {
  els.status.textContent = text || "";
  els.status.className = isError ? "err" : "";
}

function normalizeServer(raw) {
  var s = String(raw || "").trim().replace(/\/+$/, "");
  return /^https?:\/\/[^\s]+$/i.test(s) ? s : "";
}

function formatTime(ts) {
  try {
    return new Date(ts).toLocaleString("zh-CN", { hour12: false });
  } catch (e) {
    return String(ts);
  }
}

function addRow(list, label, value, cls) {
  var dt = document.createElement("dt");
  dt.textContent = label;
  var dd = document.createElement("dd");
  dd.textContent = value;
  if (cls) dd.className = cls;
  list.appendChild(dt);
  list.appendChild(dd);
}

function renderResult(result) {
  els.resultBody.innerHTML = "";

  if (!result) {
    els.resultAt.textContent = "最近一次上报：暂无";
    els.resultBody.className = "empty";
    els.resultBody.textContent = "还没有上报记录。";
    return;
  }

  var scope = [];
  if (result.query) scope.push(result.query);
  if (result.city) scope.push(result.city);
  els.resultAt.textContent = "最近一次上报：" + formatTime(result.at) + (scope.length ? " · " + scope.join(" · ") : "");
  els.resultBody.className = "";

  var list = document.createElement("dl");
  addRow(list, "结果", result.ok ? "成功" : "失败", result.ok ? "ok" : "fail");

  if (result.ok) {
    // 兼容两种统计口径：契约里的 added/updated/duplicates/total，
    // 以及服务端实际返回的 received/mapped/dropped。
    [
      ["added", "新增"],
      ["updated", "更新"],
      ["duplicates", "重复"],
      ["received", "接收"],
      ["mapped", "映射"],
      ["dropped", "丢弃"],
      ["total", "总数"]
    ].forEach(function (pair) {
      if (typeof result[pair[0]] === "number") addRow(list, pair[1], String(result[pair[0]]));
    });
  } else if (result.message) {
    addRow(list, "错误", result.message, "fail");
  }

  els.resultBody.appendChild(list);
}

async function saveConfig() {
  var server = normalizeServer(els.serverUrl.value);
  var token = els.token.value.trim();

  if (!server) {
    setStatus("地址必须以 http:// 或 https:// 开头", true);
    return;
  }
  if (!token) {
    setStatus("token 不能为空", true);
    return;
  }

  els.serverUrl.value = server;
  try {
    await chrome.storage.local.set({ serverUrl: server, token: token });
    setStatus("已保存，回到 BOSS 页面刷新后开始采集。");
  } catch (e) {
    setStatus("保存失败：" + (e && e.message ? e.message : "未知错误"), true);
  }
}

async function testConnection() {
  var server = normalizeServer(els.serverUrl.value);
  if (!server) {
    setStatus("请先填写合法的服务端地址", true);
    return;
  }

  setStatus("正在连接…");
  var controller = new AbortController();
  var timer = setTimeout(function () { controller.abort(); }, 8000);

  try {
    var response = await fetch(server + "/api/health", {
      method: "GET",
      signal: controller.signal,
      credentials: "omit",
      cache: "no-store"
    });
    var text = await response.text();
    var version = "";
    try {
      var info = JSON.parse(text);
      if (info && info.version) version = "（version " + info.version + "）";
    } catch (e) { /* 非 JSON 健康信息，忽略 */ }

    if (response.ok) setStatus("连接成功：HTTP " + response.status + version);
    else setStatus("服务可达，但返回 HTTP " + response.status + version, true);
  } catch (err) {
    if (err && err.name === "AbortError") setStatus("连接超时", true);
    else setStatus("连接失败：网络错误，或该主机未在 host_permissions 中", true);
  } finally {
    clearTimeout(timer);
  }
}

async function init() {
  try {
    var cfg = await chrome.storage.local.get(["serverUrl", "token", "lastResult"]);
    els.serverUrl.value = cfg.serverUrl || "";
    els.token.value = cfg.token || "";
    renderResult(cfg.lastResult || null);
  } catch (e) {
    setStatus("读取配置失败", true);
  }
}

els.save.addEventListener("click", saveConfig);
els.test.addEventListener("click", testConnection);
els.token.addEventListener("keydown", function (event) {
  if (event.key === "Enter") saveConfig();
});

chrome.storage.onChanged.addListener(function (changes, area) {
  if (area !== "local" || !changes.lastResult) return;
  renderResult(changes.lastResult.newValue || null);
});

init();
