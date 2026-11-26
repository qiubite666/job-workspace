/**
 * popup.js —— 配置服务端地址 / token，分别展示最近一次「列表上报」与「详情上报」结果。
 */
"use strict";

var els = {
  serverUrl: document.getElementById("serverUrl"),
  token: document.getElementById("token"),
  save: document.getElementById("save"),
  test: document.getElementById("test"),
  status: document.getElementById("status")
};

var slots = {
  list: {
    label: "列表上报",
    at: document.getElementById("listAt"),
    body: document.getElementById("listBody"),
    empty: "还没有列表上报记录。"
  },
  detail: {
    label: "详情上报",
    at: document.getElementById("detailAt"),
    body: document.getElementById("detailBody"),
    empty: "还没有详情上报记录。"
  }
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

function renderResult(slot, result) {
  slot.body.innerHTML = "";

  if (!result) {
    slot.at.textContent = slot.label + "：暂无";
    slot.body.className = "empty";
    slot.body.textContent = slot.empty;
    return;
  }

  var scope = [];
  if (result.query) scope.push(result.query);
  if (result.city) scope.push(result.city);
  slot.at.textContent = slot.label + "：" + formatTime(result.at) + (scope.length ? " · " + scope.join(" · ") : "");
  slot.body.className = "";

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

  slot.body.appendChild(list);
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
      var bits = [];
      if (info && info.version) bits.push("version " + info.version);
      if (info && typeof info.jobCount === "number") bits.push("岗位 " + info.jobCount + " 条");
      if (bits.length) version = "（" + bits.join("，") + "）";
    } catch (e) { /* 非 JSON 健康信息，忽略 */ }

    if (response.ok) {
      // 测试通过就顺手落盘：否则用户以为配好了，后台读取 storage 仍是空，
      // 下一次上报会报「未配置服务端地址或 token」。
      var token = els.token.value.trim();
      if (token) {
        await chrome.storage.local.set({ serverUrl: server, token: token });
        setStatus("连接成功：HTTP " + response.status + version + "，配置已保存");
      } else {
        setStatus("连接成功：HTTP " + response.status + version + "，但 token 为空，请填写后点保存", true);
      }
    } else {
      setStatus("服务可达，但返回 HTTP " + response.status + version, true);
    }
  } catch (err) {
    if (err && err.name === "AbortError") setStatus("连接超时", true);
    else setStatus("连接失败：网络错误，或该主机未在 host_permissions 中", true);
  } finally {
    clearTimeout(timer);
  }
}

async function init() {
  try {
    var cfg = await chrome.storage.local.get(["serverUrl", "token", "lastListResult", "lastDetailResult"]);
    els.serverUrl.value = cfg.serverUrl || "";
    els.token.value = cfg.token || "";
    renderResult(slots.list, cfg.lastListResult || null);
    renderResult(slots.detail, cfg.lastDetailResult || null);
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
  if (area !== "local") return;
  if (changes.lastListResult) renderResult(slots.list, changes.lastListResult.newValue || null);
  if (changes.lastDetailResult) renderResult(slots.detail, changes.lastDetailResult.newValue || null);
});

init();
