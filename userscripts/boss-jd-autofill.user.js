// ==UserScript==
// @name         BOSS 职位描述补全
// @namespace    jobworkspace.local
// @version      2.1.0
// @description  在 BOSS 列表页按间隔自动打开待补岗位，详情页截获页面自己的请求并写回职位库
// @match        https://www.zhipin.com/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        unsafeWindow
// @connect      job.qiubite.fun
// @connect      *
// @run-at       document-start
// ==/UserScript==

/**
 * 两个模式，同一个文件：
 *
 * 列表页（driver）：读取服务端"待补岗位"，按设定间隔依次打开对应卡片链接。
 * 详情页（capture）：截获页面自己发出的详情响应，原样上报后自动关闭标签页。
 *
 * 为什么不自己拼接口地址：BOSS 的详情接口除了 securityId 还要别的参数，
 * 自己拼会 404；而页面自己发的那次请求一定是对的，截获它最稳。
 */
(function () {
  "use strict";

  var DEFAULTS = { server: "https://job.qiubite.fun", token: "", interval: 12, limit: 100 };
  var cfg = Object.assign({}, DEFAULTS, GM_getValue("cfg", {}));

  var IS_DETAIL = /\/job_detail\//.test(location.pathname);
  var DETAIL_API = "/wapi/zpgeek/job/detail.json";

  if (IS_DETAIL) {
    // 必须在 document-start 挂钩：详情请求往往在 DOM 就绪前就发出去了
    captureMode();
    return;
  }

  /* ==================== 详情页：截获并上报 ==================== */
  function captureMode() {
    var jobId = (location.pathname.match(/\/job_detail\/([A-Za-z0-9_-]+)\.html/) || [])[1] || "";
    if (!jobId) return;

    var posted = false;

    function report(raw) {
      if (posted) return;
      posted = true;

      GM_xmlhttpRequest({
        method: "POST",
        url: cfg.server + "/api/ingest",
        headers: { "Content-Type": "application/json", "X-Ingest-Token": cfg.token },
        data: JSON.stringify({ platform: "boss", kind: "detail", jobId: jobId, raw: raw }),
        timeout: 20000,
        onload: function (res) {
          var ok = res.status === 200 && /"ok"\s*:\s*true/.test(res.responseText);
          badge(ok ? "已补全" : "上报失败 " + res.status, ok);
          window.setTimeout(closeSelf, ok ? 1500 : 4000);
        },
        onerror: function () { badge("网络错误", false); window.setTimeout(closeSelf, 4000); },
        ontimeout: function () { badge("上报超时", false); window.setTimeout(closeSelf, 4000); }
      });
    }

    function closeSelf() {
      // 只对脚本打开的标签页安全：用户手动打开的窗口 close 会被浏览器忽略
      try { window.close(); } catch (e) { /* 忽略 */ }
    }

    function badge(text, ok) {
      if (!document.body) return;   // document-start 阶段可能还没有 body
      var d = document.createElement("div");
      d.textContent = "JD 补全：" + text;
      d.style.cssText = "position:fixed;top:12px;left:50%;transform:translateX(-50%);z-index:2147483000;"
        + "padding:8px 14px;border-radius:999px;font:13px/1.4 -apple-system,sans-serif;color:#fff;"
        + "background:" + (ok ? "#137333" : "#b3261e") + ";box-shadow:0 6px 20px rgba(0,0,0,.2)";
      document.body.appendChild(d);
      window.setTimeout(function () { d.remove(); }, 4000);
    }

    // 10 秒内没截到详情请求，给出明确失败提示，避免看起来"什么都没发生"
    window.setTimeout(function () {
      if (!posted) badge("未捕获到详情请求（可能职位已下线）", false);
    }, 10000);

    // 截获页面 fetch（只旁听，不改变原响应）
    try {
      var rawFetch = unsafeWindow.fetch;
      if (typeof rawFetch === "function") {
        unsafeWindow.fetch = function () {
          var p = rawFetch.apply(this, arguments);
          try {
            var url = String(arguments[0] && arguments[0].url ? arguments[0].url : arguments[0] || "");
            if (url.indexOf(DETAIL_API) !== -1) {
              p.then(function (res) {
                res.clone().json().then(report).catch(function () {});
              }).catch(function () {});
            }
          } catch (e) { /* 忽略 */ }
          return p;
        };
      }
    } catch (e) { /* 权限不足时静默降级 */ }

    // 兜底：少数实现走 XHR
    try {
      var XO = unsafeWindow.XMLHttpRequest;
      if (XO && XO.prototype) {
        var rawOpen = XO.prototype.open;
        var rawSend = XO.prototype.send;
        XO.prototype.open = function (method, url) {
          this.__jwUrl = String(url || "");
          return rawOpen.apply(this, arguments);
        };
        XO.prototype.send = function () {
          var xhr = this;
          if (String(xhr.__jwUrl || "").indexOf(DETAIL_API) !== -1) {
            xhr.addEventListener("load", function () {
              try { report(JSON.parse(xhr.responseText)); } catch (e) { /* 忽略 */ }
            });
          }
          return rawSend.apply(this, arguments);
        };
      }
    } catch (e) { /* 忽略 */ }
  }

  /* ==================== 列表页：按间隔打开 ==================== */
  var el = {};
  var state = { running: false, opened: 0, timer: 0, hrefs: [], pending: {}, seen: {} };

  // document-start 阶段还没有 body，等 DOM 就绪再挂面板
  if (document.body) buildPanel();
  else document.addEventListener("DOMContentLoaded", buildPanel, { once: true });

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
      '  <div class="jw-hint">按间隔逐个打开待补岗位，详情页补完自动关闭。只处理当前页已出现的卡片。</div>' +
      "</div>";

    var style = document.createElement("style");
    style.textContent = [
      "#jw-jd-panel{position:fixed;right:16px;bottom:16px;width:272px;z-index:2147483000;",
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
    el.stop.disabled = true;

    box.querySelector(".jw-min").addEventListener("click", function () {
      box.classList.toggle("jw-collapsed");
    });
    el.save.addEventListener("click", saveCfg);
    el.start.addEventListener("click", start);
    el.stop.addEventListener("click", stop);
    el.interval.addEventListener("change", saveCfg);
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

  function setStatus(t) { if (el.status) el.status.textContent = t; }

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
        } catch (e) { fail("解析失败"); }
      },
      onerror: function () { fail("网络错误"); },
      ontimeout: function () { fail("超时"); }
    });
  }

  /** 收集当前页卡片里属于"待补"的链接（带页面刚下发的 securityId）。 */
  function collectTargets() {
    var out = [];
    var links = document.querySelectorAll('a[href*="/job_detail/"]');
    for (var i = 0; i < links.length; i++) {
      var href = links[i].href || "";
      var m = href.match(/\/job_detail\/([A-Za-z0-9_-]+)\.html/);
      if (!m) continue;
      var id = m[1];
      if (!state.pending[id] || state.seen[id]) continue;
      state.seen[id] = true;
      out.push(href);
    }
    return out;
  }

  function start() {
    if (state.running) return;
    if (!cfg.server || !cfg.token) return setStatus("请先填写服务端与令牌并保存");

    setStatus("正在读取待补列表…");
    fetchPending(function (data) {
      state.pending = {};
      var items = data.items || [];
      for (var i = 0; i < items.length; i++) state.pending[items[i].externalId] = true;

      state.hrefs = collectTargets();
      if (!state.hrefs.length) {
        return setStatus("当前页没有待补岗位（共 " + data.total + " 条待补，换个搜索词或往下滚）");
      }

      state.running = true;
      el.start.disabled = true;
      el.stop.disabled = false;
      setStatus("本页可补 " + state.hrefs.length + " 条，开始…");
      openNext();
    }, function (err) { setStatus("读取待补列表失败：" + err); });
  }

  function openNext() {
    if (!state.running) return;

    if (!state.hrefs.length) {
      stop();
      return setStatus("本页任务完成，共打开 " + state.opened + " 个。换搜索词或滚动后再开始");
    }

    var href = state.hrefs.shift();
    state.opened++;
    setStatus("打开第 " + state.opened + " 个（剩 " + state.hrefs.length + "）");

    // 用真实链接打开：securityId 是页面刚下发的，和手动点击完全一致
    var win = window.open(href, "_blank");
    if (!win) {
      stop();
      return setStatus("浏览器拦截了弹窗，请允许本站弹出窗口后重试");
    }

    state.timer = window.setTimeout(openNext, Math.max(5, cfg.interval) * 1000);
  }

  function stop() {
    state.running = false;
    if (state.timer) { window.clearTimeout(state.timer); state.timer = 0; }
    el.start.disabled = false;
    el.stop.disabled = true;
  }
})();
