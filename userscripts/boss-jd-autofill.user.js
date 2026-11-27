// ==UserScript==
// @name         BOSS 职位描述补全
// @namespace    jobworkspace.local
// @version      1.0.0
// @description  在 BOSS 搜索结果页自动补全缺失的职位描述：读取当前页面卡片的 securityId，逐条请求详情并写回自己的职位库
// @match        https://www.zhipin.com/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @connect      job.qiubite.fun
// @connect      *
// @run-at       document-idle
// ==/UserScript==

/**
 * 为什么这么做：
 * BOSS 的详情接口需要 securityId，而它是列表下发、短时效的凭据。
 * 用库里几小时前的 securityId 重放会被判定无效（接口返回 code 37）。
 * 所以脚本只在"当前页面已经渲染出卡片"时工作——securityId 就是页面刚拿到的那个，
 * 与页面自己发请求没有任何区别。
 *
 * 它不会自己翻页、不会搜索、不会点击：你正常浏览，它把当前页缺描述的岗位补上。
 */
(function () {
  "use strict";

  var DEFAULTS = {
    server: "https://job.qiubite.fun",
    token: "",
    interval: 12,   // 秒
    limit: 100      // 单次拉取的待补数量
  };

  var cfg = Object.assign({}, DEFAULTS, GM_getValue("cfg", {}));

  var state = {
    running: false,
    done: 0,
    failed: 0,
    skipped: 0,
    queue: [],
    seen: {},        // 本次会话已处理过的岗位 id
    pending: {},     // 服务端说的待补岗位 id
    timer: 0,
    failStreak: 0,
    lastError: ""
  };

  /* ---------------- 界面 ---------------- */
  var el = {};

  function buildPanel() {
    var box = document.createElement("div");
    box.id = "jw-jd-panel";
    box.innerHTML =
      '<div class="jw-hd"><span>JD 补全</span><button type="button" class="jw-min" title="收起">—</button></div>' +
      '<div class="jw-bd">' +
      '  <label>服务端<input type="text" id="jw-server" placeholder="https://job.qiubite.fun"></label>' +
      '  <label>令牌<input type="password" id="jw-token" placeholder="X-Ingest-Token"></label>' +
      '  <label>间隔（秒）<input type="number" id="jw-interval" min="5" max="120" step="1"></label>' +
      '  <div class="jw-row">' +
      '    <button type="button" id="jw-save">保存</button>' +
      '    <button type="button" id="jw-start" class="jw-primary">开始</button>' +
      '    <button type="button" id="jw-stop">停止</button>' +
      '  </div>' +
      '  <div class="jw-status" id="jw-status">未运行</div>' +
      '  <div class="jw-hint">仅在当前已渲染的卡片上工作；向下滚动出现新卡片会自动接着补。</div>' +
      "</div>";

    var style = document.createElement("style");
    style.textContent = [
      "#jw-jd-panel{position:fixed;right:16px;bottom:16px;width:268px;z-index:2147483000;",
      "background:#fff;color:#1b1b1f;border:1px solid rgba(0,0,0,.12);border-radius:14px;",
      "box-shadow:0 12px 32px rgba(0,0,0,.16);font:13px/1.5 -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif}",
      "#jw-jd-panel .jw-hd{display:flex;align-items:center;justify-content:space-between;",
      "padding:10px 12px;border-bottom:1px solid rgba(0,0,0,.06);font-weight:600}",
      "#jw-jd-panel .jw-min{border:0;background:transparent;font-size:16px;cursor:pointer;color:#6b7280}",
      "#jw-jd-panel .jw-bd{padding:10px 12px 12px}",
      "#jw-jd-panel.jw-collapsed .jw-bd{display:none}",
      "#jw-jd-panel label{display:block;margin-bottom:8px;color:#6b7280;font-size:12px}",
      "#jw-jd-panel input{display:block;width:100%;margin-top:4px;padding:7px 9px;font-size:14px;",
      "border:1px solid rgba(0,0,0,.14);border-radius:8px;box-sizing:border-box;background:#fff;color:#1b1b1f}",
      "#jw-jd-panel .jw-row{display:flex;gap:6px;margin:10px 0 8px}",
      "#jw-jd-panel .jw-row button{flex:1;min-height:34px;border:1px solid rgba(0,0,0,.14);",
      "border-radius:8px;background:#fff;cursor:pointer;font-size:13px}",
      "#jw-jd-panel .jw-row button.jw-primary{background:#1f6feb;border-color:#1f6feb;color:#fff}",
      "#jw-jd-panel .jw-row button:disabled{opacity:.5;cursor:not-allowed}",
      "#jw-jd-panel .jw-status{font-size:12px;color:#374151;min-height:18px;word-break:break-all}",
      "#jw-jd-panel .jw-hint{margin-top:6px;font-size:11px;color:#9ca3af;line-height:1.5}"
    ].join("");

    document.head.appendChild(style);
    document.body.appendChild(box);

    el.panel = box;
    el.server = box.querySelector("#jw-server");
    el.token = box.querySelector("#jw-token");
    el.interval = box.querySelector("#jw-interval");
    el.start = box.querySelector("#jw-start");
    el.stop = box.querySelector("#jw-stop");
    el.save = box.querySelector("#jw-save");
    el.status = box.querySelector("#jw-status");

    el.server.value = cfg.server || "";
    el.token.value = cfg.token || "";
    el.interval.value = cfg.interval || 12;

    box.querySelector(".jw-min").addEventListener("click", function () {
      box.classList.toggle("jw-collapsed");
    });

    el.save.addEventListener("click", saveCfg);
    el.start.addEventListener("click", start);
    el.stop.addEventListener("click", stop);
  }

  function saveCfg() {
    cfg.server = String(el.server.value || "").trim().replace(/\/+$/, "");
    cfg.token = String(el.token.value || "").trim();
    var n = parseInt(el.interval.value, 10);
    cfg.interval = isNaN(n) ? 12 : Math.min(Math.max(n, 5), 120);
    el.interval.value = cfg.interval;
    GM_setValue("cfg", cfg);
    setStatus("已保存");
  }

  function setStatus(text) {
    if (el.status) el.status.textContent = text;
  }

  /* ---------------- 数据 ---------------- */

  /** 从页面上已渲染的卡片里收集 {id, securityId}。 */
  function collectCards() {
    var out = [];
    var links = document.querySelectorAll('a[href*="/job_detail/"]');
    for (var i = 0; i < links.length; i++) {
      var href = links[i].getAttribute("href") || "";
      var m = href.match(/\/job_detail\/([A-Za-z0-9_-]+)\.html/);
      if (!m) continue;

      var sid = "";
      try {
        sid = new URL(href, location.href).searchParams.get("securityId") || "";
      } catch (e) {
        sid = "";
      }
      if (!sid) continue;

      out.push({ id: m[1], securityId: sid });
    }
    return out;
  }

  /** 拉取当前账号待补岗位（缺描述且有 securityId 的记录）。 */
  function fetchPending(done, fail) {
    GM_xmlhttpRequest({
      method: "GET",
      url: cfg.server + "/api/me/library/pending-jd?limit=" + encodeURIComponent(cfg.limit),
      headers: { "X-Ingest-Token": cfg.token },
      timeout: 15000,
      onload: function (res) {
        if (res.status !== 200) return fail("HTTP " + res.status);
        try {
          var data = JSON.parse(res.responseText);
          if (!data || !data.ok) return fail((data && data.error) || "BAD_RESPONSE");
          done(data);
        } catch (e) {
          fail("解析失败");
        }
      },
      onerror: function () { fail("网络错误"); },
      ontimeout: function () { fail("超时"); }
    });
  }

  function postDetail(jobId, raw, done, fail) {
    GM_xmlhttpRequest({
      method: "POST",
      url: cfg.server + "/api/ingest",
      headers: {
        "Content-Type": "application/json",
        "X-Ingest-Token": cfg.token
      },
      data: JSON.stringify({ platform: "boss", kind: "detail", jobId: jobId, raw: raw }),
      timeout: 20000,
      onload: function (res) {
        if (res.status !== 200) return fail("入库 HTTP " + res.status);
        try {
          var data = JSON.parse(res.responseText);
          if (!data || !data.ok) return fail((data && data.error) || "INGEST_FAILED");
          done(data);
        } catch (e) {
          fail("入库响应解析失败");
        }
      },
      onerror: function () { fail("入库网络错误"); },
      ontimeout: function () { fail("入库超时"); }
    });
  }

  /* ---------------- 调度 ---------------- */

  function scan() {
    if (!state.running) return;

    var cards = collectCards();
    var added = 0;
    for (var i = 0; i < cards.length; i++) {
      var c = cards[i];
      if (state.seen[c.id]) continue;
      if (!state.pending[c.id]) continue;   // 服务端说已经补过的不动
      state.seen[c.id] = true;
      state.queue.push(c);
      added++;
    }

    if (added) {
      setStatus("新发现 " + added + " 条可补，队列 " + state.queue.length);
    }
    pump();
  }

  function pump() {
    if (!state.running) return;
    if (state.timer) return;          // 正在等待间隔
    if (!state.queue.length) {
      setStatus("待补 " + state.done + " 条 · 当前页面暂无可补岗位，滚动列表试试");
      return;
    }

    var item = state.queue.shift();
    setStatus("请求中：" + item.id + "（剩 " + state.queue.length + " 条）");

    requestDetail(item.securityId, function (raw) {
      postDetail(item.id, raw, function (data) {
        state.done++;
        state.failStreak = 0;
        setStatus("已完成 " + state.done + " 条" + (state.failed ? "，失败 " + state.failed : ""));
        scheduleNext();
      }, function (err) {
        onFail("入库失败 " + err);
      });
    }, function (err) {
      onFail(err);
    });
  }

  function requestDetail(securityId, done, fail) {
    var url = "/wapi/zpgeek/job/detail.json?securityId=" + encodeURIComponent(securityId);
    fetch(url, { credentials: "include", headers: { Accept: "application/json" } })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (data) {
        if (!data || (typeof data.code === "number" && data.code !== 0)) {
          throw new Error("接口 code " + (data && data.code) + (data && data.message ? "：" + data.message : ""));
        }
        done(data);
      })
      .catch(function (e) { fail(String(e && e.message ? e.message : e)); });
  }

  function onFail(msg) {
    state.failed++;
    state.failStreak++;
    state.lastError = msg;

    if (state.failStreak >= 3) {
      stop();
      setStatus("已自动停止：连续 3 次失败（" + msg + "）");
      return;
    }
    setStatus("失败 " + state.failed + " 次（" + msg + "）");
    scheduleNext();
  }

  function scheduleNext() {
    if (!state.running) return;
    var ms = Math.max(5, cfg.interval) * 1000;
    state.timer = window.setTimeout(function () {
      state.timer = 0;
      pump();
    }, ms);
  }

  function start() {
    if (state.running) return;
    if (!cfg.server || !cfg.token) {
      setStatus("请先填写服务端与令牌并保存");
      return;
    }

    setStatus("正在读取待补列表…");
    fetchPending(function (data) {
      state.pending = {};
      var items = data.items || [];
      for (var i = 0; i < items.length; i++) {
        state.pending[items[i].externalId] = true;
      }

      state.running = true;
      state.failStreak = 0;
      el.start.disabled = true;
      el.stop.disabled = false;

      setStatus("待补 " + data.total + " 条，开始扫描当前页…");
      window.setInterval(function () {
        if (state.running) scan();
      }, 3000);
      scan();

      // 页面滚动/加载新卡片时也扫一次
      window.addEventListener("scroll", throttledScan, { passive: true });
    }, function (err) {
      setStatus("读取待补列表失败：" + err);
    });
  }

  var scanTimer = 0;
  function throttledScan() {
    if (!state.running || scanTimer) return;
    scanTimer = window.setTimeout(function () {
      scanTimer = 0;
      scan();
    }, 1200);
  }

  function stop() {
    state.running = false;
    if (state.timer) {
      window.clearTimeout(state.timer);
      state.timer = 0;
    }
    el.start.disabled = false;
    el.stop.disabled = true;
  }

  /* ---------------- 启动 ---------------- */
  function boot() {
    buildPanel();
    el.stop.disabled = true;
    el.interval.addEventListener("change", saveCfg);
    el.server.addEventListener("change", saveCfg);
    el.token.addEventListener("change", saveCfg);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
