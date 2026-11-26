/**
 * background.js —— MV3 service worker。
 * 接收 relay 转来的岗位数据，补齐配置后 POST 到 {serverUrl}/api/ingest，并把结果写回 badge 与 storage。
 *
 * 上报分两类，靠 payload.kind 分派（content.js 判定，服务端按 kind 走不同映射链路）：
 *   - list  ：列表/推荐接口，raw.zpData 是岗位数组；成功后 badge 显示新增条数；
 *   - detail：职位详情接口，raw.zpData.jobInfo 是单个职位；成功后 badge 显示 "+更新数"。
 * 两条链路的结果分别落盘（lastListResult / lastDetailResult），popup 各展示一条。
 */
"use strict";

var KIND_LIST = "list";
var KIND_DETAIL = "detail";

var BADGE_CLEAR_MS = 3000;
var REQUEST_TIMEOUT_MS = 15000;
var MAX_BODY_BYTES = 4 * 1024 * 1024; // 与服务端 ingestBodyLimit 一致

var badgeTimer = null;

function normalizeKind(kind) {
  return String(kind || "").trim().toLowerCase() === KIND_DETAIL ? KIND_DETAIL : KIND_LIST;
}

function setBadge(text, color) {
  try {
    chrome.action.setBadgeText({ text: text });
    if (color) chrome.action.setBadgeBackgroundColor({ color: color });
  } catch (e) { /* 忽略 */ }
}

function flashSuccess(count) {
  if (badgeTimer) clearTimeout(badgeTimer);
  setBadge(String(count), "#2e7d32");
  badgeTimer = setTimeout(function () {
    badgeTimer = null;
    setBadge("", null);
  }, BADGE_CLEAR_MS);
}

function markFailure() {
  if (badgeTimer) { clearTimeout(badgeTimer); badgeTimer = null; }
  setBadge("x", "#c62828");
}

function normalizeServer(raw) {
  var s = String(raw || "").trim().replace(/\/+$/, "");
  return /^https?:\/\/[^\s]+$/i.test(s) ? s : "";
}

function isNonEmptyObject(value) {
  if (!value || typeof value !== "object") return false;
  if (Object.prototype.toString.call(value) === "[object Array]") return value.length > 0;
  return Object.keys(value).length > 0;
}

function isValidPayload(payload) {
  if (!payload || typeof payload !== "object") return false;
  if (payload.platform !== "boss") return false;
  if (!payload.raw || typeof payload.raw !== "object") return false;
  var zp = payload.raw.zpData;
  // 详情：只接受含非空 jobInfo 的响应（content.js 已先挡一层，这里再挡一次）。
  if (normalizeKind(payload.kind) === KIND_DETAIL) return isNonEmptyObject(zp && zp.jobInfo);
  return isNonEmptyObject(zp);
}

/** 两类上报的结果分开存，避免详情刷新把列表结果覆盖掉。 */
function resultStorageKey(kind) {
  return normalizeKind(kind) === KIND_DETAIL ? "lastDetailResult" : "lastListResult";
}

/** 最近一次上报结果，供 popup 按 kind 展示；错误文本会一直保留到下一次同类上报。 */
function recordResult(kind, result) {
  result.kind = normalizeKind(kind);
  result.at = result.at || Date.now();
  var patch = {};
  patch[resultStorageKey(kind)] = result;
  try {
    return chrome.storage.local.set(patch);
  } catch (e) {
    return Promise.resolve();
  }
}

function errorText(err) {
  if (!err) return "未知错误";
  if (err.name === "AbortError") return "请求超时（" + Math.round(REQUEST_TIMEOUT_MS / 1000) + " 秒）";
  return err.message || String(err);
}

async function postToServer(server, token, payload) {
  // kind 显式带上：服务端据此把 raw 分派到列表映射或详情映射。缺省时服务端按 list 处理，
  // 但扩展这边一律写死，避免上游漏传导致详情被当成列表丢掉。
  var envelope = { platform: "boss", kind: normalizeKind(payload.kind), raw: payload.raw };
  if (payload.query) envelope.query = payload.query;
  if (payload.city) envelope.city = payload.city;
  var body = JSON.stringify(envelope);
  var bodyBytes = new TextEncoder().encode(body).length;

  if (bodyBytes > MAX_BODY_BYTES) {
    throw new Error("响应体超过 4MB 上限（" + bodyBytes + " 字节），已跳过本批");
  }

  var controller = new AbortController();
  var timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);

  try {
    return await fetch(server + "/api/ingest", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Ingest-Token": token
      },
      body: body,
      signal: controller.signal,
      credentials: "omit",
      cache: "no-store"
    });
  } finally {
    clearTimeout(timer);
  }
}

async function handleIngest(payload) {
  if (!isValidPayload(payload)) {
    return { ok: false, error: "INVALID_PAYLOAD" };
  }

  var kind = normalizeKind(payload.kind);
  var cfg = await chrome.storage.local.get(["serverUrl", "token"]);
  var server = normalizeServer(cfg.serverUrl);
  var token = String(cfg.token || "").trim();

  if (!server || !token) {
    setBadge("!", "#f9a825");
    await recordResult(kind, {
      ok: false,
      error: "NOT_CONFIGURED",
      message: "未配置服务端地址或 token，请在扩展弹窗里填写",
      query: payload.query || "",
      city: payload.city || ""
    });
    return { ok: false, error: "NOT_CONFIGURED" };
  }

  var response;
  try {
    response = await postToServer(server, token, payload);
  } catch (err) {
    markFailure();
    await recordResult(kind, {
      ok: false,
      error: "NETWORK",
      message: "请求失败：" + errorText(err),
      query: payload.query || "",
      city: payload.city || ""
    });
    return { ok: false, error: "NETWORK" };
  }

  var data = null;
  try {
    data = await response.json();
  } catch (e) {
    data = null;
  }

  if (response.status === 401) {
    markFailure();
    await recordResult(kind, {
      ok: false,
      status: 401,
      error: "UNAUTHORIZED",
      message: "鉴权失败（401）：token 不正确或已失效",
      query: payload.query || "",
      city: payload.city || ""
    });
    return { ok: false, status: 401, error: "UNAUTHORIZED" };
  }

  if (!response.ok || !data || data.ok !== true) {
    markFailure();
    await recordResult(kind, {
      ok: false,
      status: response.status,
      error: (data && data.error) || "HTTP_" + response.status,
      message: "上报失败（HTTP " + response.status + "）" + (data && data.error ? "：" + data.error : ""),
      query: payload.query || "",
      city: payload.city || ""
    });
    return { ok: false, status: response.status };
  }

  // 服务端可能返回 added/updated/duplicates/total，也可能返回 received/mapped/dropped；
  // 两边都收，原样写入该 kind 的结果槽位，由 popup 按存在的字段展示。
  var stats = {};
  ["added", "updated", "duplicates", "total", "received", "mapped", "dropped"].forEach(function (key) {
    if (typeof data[key] === "number" && isFinite(data[key])) stats[key] = data[key];
  });

  var added = stats.added || 0;
  var updated = stats.updated || 0;

  // badge：列表看新增条数，详情看服务端回写的 updated（一个职位通常就是 0 或 1）。
  flashSuccess(kind === KIND_DETAIL ? "+" + updated : String(added));
  await recordResult(kind, Object.assign({
    ok: true,
    status: response.status,
    query: payload.query || "",
    city: payload.city || ""
  }, stats));
  return Object.assign({ ok: true }, stats);
}

chrome.runtime.onMessage.addListener(function (message, sender, sendResponse) {
  if (!message || message.type !== "BOSS_INGEST_RELAY") return false;

  handleIngest(message.payload).then(sendResponse, function (err) {
    markFailure();
    recordResult(message.payload && message.payload.kind, {
      ok: false,
      error: "UNEXPECTED",
      message: "未预期错误：" + errorText(err)
    });
    sendResponse({ ok: false, error: "UNEXPECTED" });
  });
  return true; // 异步响应
});

chrome.runtime.onInstalled.addListener(function () {
  setBadge("", null);
});
