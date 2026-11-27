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
  if (payload.jobId) envelope.jobId = payload.jobId;
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
  if (message && message.type === "BOSS_AUTOFILL_START") {
    startAutofill(message.limit).then(sendResponse, function (err) {
      sendResponse({ ok: false, error: "START_FAILED", message: String(err) });
    });
    return true;
  }
  if (message && message.type === "BOSS_AUTOFILL_STOP") {
    autofill.stop = true;
    sendResponse({ ok: true });
    return false;
  }

  if (!message || message.type !== "BOSS_INGEST_RELAY") return false;

  if (message.payload && message.payload.kind === "detail" && message.payload.sourceUrl) {
    // 记住最近一次成功的详情请求地址，作为批量补全的模板
    chrome.storage.local.set({ lastDetailSource: message.payload.sourceUrl });
  }

  chrome.storage.local.set({
    lastRelay: {
      kind: (message.payload && message.payload.kind) || "",
      jobId: (message.payload && message.payload.jobId) || "",
      at: Date.now()
    }
  });

  chrome.storage.local.set({
    lastRelay: {
      kind: (message.payload && message.payload.kind) || "",
      jobId: (message.payload && message.payload.jobId) || "",
      at: Date.now()
    }
  });

  handleIngest(message.payload).then(function (res) {
    sendResponse(res);
  }, function (err) {
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

/* =========================================================
   批量补 JD（重放模式）
   ---------------------------------------------------------
   直接访问职位页不会触发详情请求，直接调接口又缺参数。
   折中做法：用用户手动浏览时抓到的"真实请求地址"作模板，
   只把 securityId 换成目标岗位的，再在同一个页面里发出去——
   参数、来源、登录态都与真实请求一致。
   ========================================================= */
var autofill = { running: false, done: 0, total: 0, failed: 0, reason: "", stop: false, lastError: "" };

var AUTOFILL_MIN_GAP_MS = 6000;
var AUTOFILL_JITTER_MS = 3000;
var AUTOFILL_MAX_BATCH = 50;

function sleep(ms) {
  return new Promise(function (resolve) { setTimeout(resolve, ms); });
}

async function publishAutofill() {
  await chrome.storage.local.set({
    autofillState: {
      running: autofill.running,
      done: autofill.done,
      total: autofill.total,
      failed: autofill.failed,
      reason: autofill.reason,
      lastError: autofill.lastError
    }
  });
}

async function fetchPending(server, token, limit) {
  var response = await fetch(server + "/api/me/library/pending-jd?limit=" + encodeURIComponent(limit), {
    headers: { "X-Ingest-Token": token }
  });
  if (!response.ok) throw new Error("HTTP " + response.status);
  return response.json();
}

async function pickBossTab() {
  var tabs = await chrome.tabs.query({ url: "https://www.zhipin.com/*" });
  return tabs && tabs.length ? tabs[0] : null;
}

async function startAutofill(limit) {
  if (autofill.running) return { ok: false, error: "RUNNING" };

  var cfg = await chrome.storage.local.get(["serverUrl", "token", "lastDetailSource", "autofillGapMs"]);
  var server = normalizeServer(cfg.serverUrl);
  var token = String(cfg.token || "").trim();
  if (!server || !token) return { ok: false, error: "NOT_CONFIGURED" };

  // 没有模板就没法重放：先让用户手动点开一个职位，扩展才能学到请求长什么样。
  var template = String(cfg.lastDetailSource || "").trim();
  if (!template) return { ok: false, error: "NEED_MANUAL" };

  var tab = await pickBossTab();
  if (!tab) return { ok: false, error: "NO_BOSS_TAB" };

  var batch = Math.min(Math.max(parseInt(limit, 10) || 20, 1), AUTOFILL_MAX_BATCH);

  var pending;
  try {
    pending = await fetchPending(server, token, batch);
  } catch (err) {
    return { ok: false, error: "FETCH_FAILED", message: String(err && err.message ? err.message : err) };
  }

  var items = (pending && Array.isArray(pending.items)) ? pending.items : [];
  if (!items.length) return { ok: false, error: "NOTHING_TO_DO" };

  autofill = { running: true, done: 0, total: items.length, failed: 0, reason: "", stop: false, lastError: "" };
  await publishAutofill();

  // 间隔默认 6-9 秒随机；显式配置时用它（自测与特殊场景需要调快）。
  var gapMs = Number(cfg.autofillGapMs);
  runAutofill(server, token, tab.id, template, items, gapMs > 0 ? gapMs : 0);
  return { ok: true, total: items.length };
}

async function runAutofill(server, token, tabId, template, items, gapMs) {
  var consecutiveFail = 0;

  for (var i = 0; i < items.length; i++) {
    if (autofill.stop) { autofill.reason = "STOPPED"; break; }

    var item = items[i];
    try {
      var result = await chrome.tabs.sendMessage(tabId, {
        type: "BOSS_AUTOFILL_FETCH",
        template: template,
        securityId: String(item.securityId || item.securityID || "").trim()
      });

      var blocked = !result || !result.ok;
      if (!blocked && result.data && typeof result.data.code === "number" && result.data.code !== 0) {
        blocked = true;
        autofill.lastError = "API_CODE_" + result.data.code;
      }

      if (blocked) {
        consecutiveFail++;
        autofill.failed++;
        if (!autofill.lastError) autofill.lastError = (result && (result.error || ("HTTP " + result.status))) || "UNKNOWN";
      } else {
        consecutiveFail = 0;
        // platform 不能少：isValidPayload 会直接拒掉，表现为"进度在走但没有入库"
        await handleIngest({
          platform: "boss",
          kind: "detail",
          jobId: item.externalId,
          raw: result.data,
          query: "",
          city: ""
        });
      }
    } catch (err) {
      consecutiveFail++;
      autofill.failed++;
      autofill.lastError = String(err && err.message ? err.message : err);
    }

    autofill.done++;
    await publishAutofill();

    if (consecutiveFail >= 3) { autofill.reason = "BLOCKED"; break; }
    if (i < items.length - 1 && !autofill.stop) {
      await sleep(gapMs || (AUTOFILL_MIN_GAP_MS + Math.random() * AUTOFILL_JITTER_MS));
    }
  }

  autofill.running = false;
  if (!autofill.reason) autofill.reason = "DONE";
  await publishAutofill();
}
