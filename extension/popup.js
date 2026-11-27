/**
 * popup.js —— 配置服务端地址 / token，分别展示最近一次「列表上报」与「详情上报」结果。
 */
"use strict";

var els = {
  serverUrl: document.getElementById("serverUrl"),
  token: document.getElementById("token"),
  save: document.getElementById("save"),
  test: document.getElementById("test"),
  status: document.getElementById("status"),
  autofillMeta: document.getElementById("autofillMeta"),
  autofillStart: document.getElementById("autofillStart"),
  autofillStop: document.getElementById("autofillStop")
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

/* ---------- 批量补 JD ---------- */
var AUTOFILL_LIMIT = 20;

function describeAutofill(state) {
  if (!state || (!state.running && !state.done)) return "批量补 JD：未运行";
  if (state.running) {
    var line = "批量补 JD：进行中 " + state.done + "/" + state.total
      + (state.failed ? "，失败 " + state.failed : "");
    if (state.lastError) line += "（" + state.lastError + "）";
    return line;
  }
  if (state.reason === "DONE") return "批量补 JD：完成 " + state.done + " 条";
  if (state.lastError && state.failed) {
    var tag = state.reason === "BLOCKED" ? "已中止" : "部分失败";
    return "批量补 JD：" + tag + "，失败 " + state.failed + " 条（" + state.lastError + "）";
  }
  if (state.reason === "BLOCKED") return "批量补 JD：已中止（连续失败，可能被限流）";
  if (state.reason === "STOPPED") return "批量补 JD：已停止，完成 " + state.done + " 条";
  return "批量补 JD：完成 " + state.done + " 条";
}

function renderAutofill(state) {
  if (!els.autofillMeta) return;
  els.autofillMeta.textContent = describeAutofill(state);
  var running = !!(state && state.running);
  if (els.autofillStart) els.autofillStart.disabled = running;
  if (els.autofillStop) els.autofillStop.disabled = !running;
}

async function startAutofill() {
  if (els.autofillStart) els.autofillStart.disabled = true;
  try {
    var res = await chrome.runtime.sendMessage({ type: "BOSS_AUTOFILL_START", limit: AUTOFILL_LIMIT });
    if (!res || !res.ok) {
      var map = {
        NOT_CONFIGURED: "请先填写服务端地址与 token",
        NO_BOSS_TAB: "请先打开一个 BOSS 页面（zhipin.com）",
        NOTHING_TO_DO: "没有可补的岗位（需要先滚动一次列表拿到 securityId）",
        NEED_MANUAL: "请先手动点开任意一个职位（让扩展学到请求格式），再开始",
        NEED_MANUAL: "请先手动点开任意一个职位（让扩展学到请求格式），再开始",
        RUNNING: "已有任务在进行",
        FETCH_FAILED: "读取待补列表失败"
      };
      els.autofillMeta.textContent = "批量补 JD：" + (map[res && res.error] || (res && res.message) || "启动失败");
      if (els.autofillStart) els.autofillStart.disabled = false;
      return;
    }
    els.autofillMeta.textContent = "批量补 JD：已启动，共 " + res.total + " 条";
  } catch (err) {
    els.autofillMeta.textContent = "批量补 JD：启动失败（" + String(err) + "）";
    if (els.autofillStart) els.autofillStart.disabled = false;
  }
}

async function stopAutofill() {
  try {
    await chrome.runtime.sendMessage({ type: "BOSS_AUTOFILL_STOP" });
  } catch (err) { /* 忽略：后台可能已停 */ }
}

async function init() {
  try {
    var el = document.getElementById("ver");
    if (el) el.textContent = "v" + chrome.runtime.getManifest().version;
  } catch (e) { /* 忽略 */ }

  try {
    var cfg = await chrome.storage.local.get(["serverUrl", "token", "lastListResult", "lastDetailResult"]);
    els.serverUrl.value = cfg.serverUrl || "";
    els.token.value = cfg.token || "";
    renderResult(slots.list, cfg.lastListResult || null);
    renderResult(slots.detail, cfg.lastDetailResult || null);
    var st = await chrome.storage.local.get(["autofillState"]);
    renderAutofill(st.autofillState || null);
  } catch (e) {
    setStatus("读取配置失败", true);
  }
}

if (els.autofillStart) els.autofillStart.addEventListener("click", startAutofill);
if (els.autofillStop) els.autofillStop.addEventListener("click", stopAutofill);

els.save.addEventListener("click", saveConfig);
els.test.addEventListener("click", testConnection);
els.token.addEventListener("keydown", function (event) {
  if (event.key === "Enter") saveConfig();
});

chrome.storage.onChanged.addListener(function (changes, area) {
  if (area !== "local") return;
  if (changes.lastListResult) renderResult(slots.list, changes.lastListResult.newValue || null);
  if (changes.lastDetailResult) renderResult(slots.detail, changes.lastDetailResult.newValue || null);
  if (changes.autofillState) renderAutofill(changes.autofillState.newValue || null);
});

init();
