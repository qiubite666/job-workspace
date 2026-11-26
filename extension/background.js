/**
 * background.js —— MV3 service worker。
 * 接收 relay 转来的岗位数据，补齐配置后 POST 到 {serverUrl}/api/ingest，并把结果写回 badge 与 storage。
 */
"use strict";

var BADGE_CLEAR_MS = 3000;
var REQUEST_TIMEOUT_MS = 15000;
var MAX_BODY_BYTES = 4 * 1024 * 1024; // 与服务端 ingestBodyLimit 一致

var badgeTimer = null;

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
  return isNonEmptyObject(payload.raw.zpData);
}

/** 最近一次上报结果，供 popup 展示；错误文本会一直保留到下一次上报。 */
function recordResult(result) {
  result.at = result.at || Date.now();
  try {
    return chrome.storage.local.set({ lastResult: result });
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
  var envelope = { platform: "boss", raw: payload.raw };
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

  var cfg = await chrome.storage.local.get(["serverUrl", "token"]);
  var server = normalizeServer(cfg.serverUrl);
  var token = String(cfg.token || "").trim();

  if (!server || !token) {
    setBadge("!", "#f9a825");
    await recordResult({
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
    await recordResult({
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
    await recordResult({
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
    await recordResult({
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
  // 两边都收，原样写入 lastResult，由 popup 按存在的字段展示。
  var stats = {};
  ["added", "updated", "duplicates", "total", "received", "mapped", "dropped"].forEach(function (key) {
    if (typeof data[key] === "number" && isFinite(data[key])) stats[key] = data[key];
  });

  var added = stats.added || 0;
  var updated = stats.updated || 0;

  flashSuccess(added + updated);
  await recordResult(Object.assign({
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
    recordResult({
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
