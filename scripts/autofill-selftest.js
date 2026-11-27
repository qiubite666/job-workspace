/**
 * 批量补全逻辑自测（不需要浏览器、不需要 BOSS 登录）。
 *
 * 做法：把 background.js 装进 Node 的 vm 沙箱，用假的 chrome.* API 与 fetch
 * 驱动一遍完整流程，断言：
 *   1. 待补列表里的 securityId 被正确读出（曾因字段名写成 securityID 而全线 BAD_ARGS）
 *   2. 每条都带着正确的 jobId 入库
 *   3. 连续失败会熔断
 *
 * 跑法：node scripts/autofill-selftest.js
 */
"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const SOURCE = fs.readFileSync(path.join(__dirname, "..", "extension", "background.js"), "utf8");

function createHarness(options) {
  const opts = options || {};
  const store = Object.assign({
    serverUrl: "https://example.test",
    token: "test-token",
    lastDetailSource: "https://www.zhipin.com/wapi/zpgeek/job/detail.json?securityId=old&lid=1",
    autofillGapMs: 5
  }, opts.store || {});

  const calls = { sendMessage: [], ingest: 0, ingestedJobIds: [] };
  let messageHandler = null;

  const chrome = {
    action: {
      setBadgeText: () => {},
      setBadgeBackgroundColor: () => {}
    },
    runtime: {
      onInstalled: { addListener: () => {} },
      onMessage: { addListener: (fn) => { messageHandler = fn; } }
    },
    storage: {
      local: {
        get: (keys) => {
          const list = Array.isArray(keys) ? keys : [keys];
          const out = {};
          list.forEach((k) => { out[k] = store[k]; });
          return Promise.resolve(out);
        },
        set: (obj) => {
          Object.assign(store, obj);
          return Promise.resolve();
        }
      }
    },
    tabs: {
      query: () => Promise.resolve([{ id: 1, url: "https://www.zhipin.com/web/geek/job" }]),
      sendMessage: (tabId, msg) => {
        calls.sendMessage.push(msg);
        if (opts.failSendMessage) return Promise.reject(new Error("tab gone"));
        return Promise.resolve({ ok: true, data: { code: 0, zpData: { jobInfo: { jobName: "x" } } } });
      }
    }
  };

  const pendingItems = opts.items || [
    { id: "boss:a1", externalId: "a1", securityId: "sid-a1", jobName: "岗位A" },
    { id: "boss:b2", externalId: "b2", securityId: "sid-b2", jobName: "岗位B" },
    { id: "boss:c3", externalId: "c3", securityId: "sid-c3", jobName: "岗位C" }
  ];

  const fetch = (url, init) => {
    const u = String(url);
    if (u.indexOf("/api/me/library/pending-jd") !== -1) {
      return Promise.resolve({
        ok: true, status: 200,
        json: () => Promise.resolve({ ok: true, total: pendingItems.length, items: pendingItems })
      });
    }
    if (u.indexOf("/api/ingest") !== -1) {
      calls.ingest += 1;
      try {
        const body = JSON.parse(init.body);
        calls.ingestedJobIds.push(body.jobId || "");
      } catch (e) { /* ignore */ }
      return Promise.resolve({
        ok: true, status: 200,
        json: () => Promise.resolve({ ok: true, added: 0, updated: 1, total: 1 })
      });
    }
    return Promise.reject(new Error("unexpected fetch " + u));
  };

  const sandbox = {
    chrome,
    fetch,
    console,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    TextEncoder,
    AbortController,
    URL,
    Promise
  };
  sandbox.globalThis = sandbox;

  vm.createContext(sandbox);
  vm.runInContext(SOURCE, sandbox, { filename: "background.js" });

  return {
    store,
    calls,
    start: (limit) => new Promise((resolve) => messageHandler({ type: "BOSS_AUTOFILL_START", limit }, {}, resolve)),
    relay: (payload) => new Promise((resolve) => messageHandler({ type: "BOSS_INGEST_RELAY", payload }, {}, resolve)),
    stop: () => messageHandler({ type: "BOSS_AUTOFILL_STOP" }, {}, () => {})
  };
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function waitUntilIdle(harness, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || 15000);
  while (Date.now() < deadline) {
    const state = harness.store.autofillState;
    if (state && state.running === false) return state;
    await sleep(50);
  }
  return harness.store.autofillState;
}

const results = [];
function check(name, ok, detail) {
  results.push({ name, ok, detail });
  console.log((ok ? "PASS " : "FAIL ") + name + (detail ? "  " + detail : ""));
}

(async () => {
  // 用例 1：正常补全 3 条
  {
    const h = createHarness();
    const start = await h.start(3);
    check("启动成功", start && start.ok === true, JSON.stringify(start));

    const state = await waitUntilIdle(h);
    check("全部完成", state && state.done === 3 && state.failed === 0,
      "done=" + (state && state.done) + " failed=" + (state && state.failed));

    const sids = h.calls.sendMessage.map((m) => m.securityId);
    check("securityId 读取正确（曾错写成 securityID）",
      sids.join(",") === "sid-a1,sid-b2,sid-c3", sids.join(","));

    check("模板被透传", h.calls.sendMessage.every((m) => String(m.template || "").indexOf("detail.json") !== -1));

    check("每条都入库且 jobId 正确",
      h.calls.ingestedJobIds.join(",") === "a1,b2,c3", h.calls.ingestedJobIds.join(","));
  }

  // 用例 2：中继上报不能抛异常（曾因残留 autofillWaiter 引用报 ReferenceError）
  {
    const h = createHarness();
    await h.relay({ platform: "boss", kind: "detail", jobId: "x1", raw: { zpData: { jobInfo: { jobName: "x" } } } });
    await sleep(50);
    check("中继上报不抛异常且能入库", h.calls.ingest === 1, "ingest=" + h.calls.ingest);
  }

  // 用例 3：无模板时应拒绝启动
  {
    const h = createHarness({ store: { lastDetailSource: "" } });
    const start = await h.start(3);
    check("无模板时提示 NEED_MANUAL", start && start.error === "NEED_MANUAL", JSON.stringify(start));
  }

  // 用例 4：连续失败熔断
  {
    const h = createHarness({ failSendMessage: true });
    await h.start(3);
    const state = await waitUntilIdle(h, 20000);
    check("连续失败后熔断", state && state.reason === "BLOCKED" && state.failed >= 3,
      "reason=" + (state && state.reason) + " failed=" + (state && state.failed));
  }

  const failed = results.filter((r) => !r.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " 通过");
  process.exit(failed.length ? 1 : 0);
})().catch((err) => {
  console.error("自测异常：", err);
  process.exit(2);
});
