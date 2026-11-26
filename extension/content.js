/**
 * content.js —— 运行在页面的 MAIN world。
 *
 * 职责只有两件：
 *   1. 旁听页面自身的 fetch / XMLHttpRequest 响应（只读，不改 URL、不改 header、不重发）；
 *   2. 命中 BOSS 岗位接口且 zpData 非空时，用 window.postMessage 交给 ISOLATED world 的 relay.js。
 *
 * 这里拿不到 chrome.* 任何 API，也刻意不接触任何登录凭据。
 */
(function () {
  "use strict";

  if (window.__BOSS_INGEST_HOOKED__) return;
  window.__BOSS_INGEST_HOOKED__ = true;

  var TARGET_PATHS = [
    "/wapi/zpgeek/search/joblist.json",
    "/wapi/zpgeek/pc/recommend/job/list.json",
    "/wapi/zpgeek/recommend/job/list.json"
  ];
  var DEDUPE_MS = 2000;
  var DEBUG = false; // 需要排查时改为 true，正常使用保持静音

  var recent = Object.create(null);

  function log() {
    if (!DEBUG) return;
    try {
      console.log.apply(console, ["[boss-ingest]"].concat([].slice.call(arguments)));
    } catch (e) { /* 忽略 */ }
  }

  /** 判断 URL 是否命中所关注的岗位接口；同时返回标准化后的键用于节流。 */
  function matchTarget(rawUrl) {
    try {
      var u = new URL(String(rawUrl || ""), location.href);
      if (!/(^|\.)zhipin\.com$/i.test(u.hostname)) return null;
      var i;
      for (i = 0; i < TARGET_PATHS.length; i += 1) {
        if (u.pathname.indexOf(TARGET_PATHS[i]) !== -1) {
          return { key: u.pathname + u.search, url: u.href };
        }
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  /** 同一 URL（含参数）在 2 秒内只处理一次，避免无限滚动时重复上报同一页。 */
  function allowOnce(key) {
    var now = Date.now();
    if (recent[key] && now - recent[key] < DEDUPE_MS) return false;
    recent[key] = now;
    var keys = Object.keys(recent);
    if (keys.length > 40) {
      var i;
      for (i = 0; i < keys.length; i += 1) {
        if (now - recent[keys[i]] > DEDUPE_MS * 5) delete recent[keys[i]];
      }
    }
    return true;
  }

  function readQuery() {
    try {
      var q = new URLSearchParams(location.search).get("query");
      if (q && q.trim()) return q.trim();
      var el = document.querySelector('input[name="query"], input.search-input, #search-input');
      if (el && el.value && el.value.trim()) return el.value.trim();
    } catch (e) { /* 忽略 */ }
    return "";
  }

  function readCity() {
    try {
      var el = document.querySelector(".city-label, .city-name, .cur-city");
      var text = el && el.textContent ? el.textContent.replace(/[\[\]\s]+/g, "") : "";
      if (text) return text;
      // 搜索页的 city 参数是数字城市码（如 101080100），不是城市名，不当成 name 上报。
      var c = new URLSearchParams(location.search).get("city");
      if (c && !/^\d+$/.test(c)) return c;
      var cityEl = document.querySelector('input[name="city"], #city');
      if (cityEl && cityEl.value) return String(cityEl.value).trim();
    } catch (e) { /* 忽略 */ }
    return "";
  }

  function hasZpData(data) {
    if (!data || typeof data !== "object") return false;
    var zp = data.zpData;
    if (!zp || typeof zp !== "object") return false;
    if (Object.prototype.toString.call(zp) === "[object Array]") return zp.length > 0;
    return Object.keys(zp).length > 0;
  }

  /** 把命中的响应体交给页面（由 relay.js 承接），不修改原响应。 */
  function emit(matched, data) {
    if (!hasZpData(data)) return;
    if (!allowOnce(matched.key)) return;
    window.postMessage(
      {
        type: "BOSS_INGEST",
        payload: {
          platform: "boss",
          query: readQuery(),
          city: readCity(),
          raw: data
        }
      },
      location.origin
    );
    log("posted", matched.key);
  }

  function hookFetch() {
    var originalFetch = window.fetch;
    if (typeof originalFetch !== "function") return;

    window.fetch = function (input) {
      var args = arguments;
      var promise = originalFetch.apply(this, args);
      try {
        var url = typeof input === "string" ? input : input && input.url ? input.url : "";
        var matched = matchTarget(url);
        if (matched && promise && typeof promise.then === "function") {
          promise.then(function (res) {
            try {
              var ct = (res.headers && res.headers.get("content-type")) || "";
              if (ct.indexOf("json") === -1) return;
              res.clone().json().then(function (data) {
                try { emit(matched, data); } catch (e) { /* 忽略 */ }
              }, function () { /* 非 JSON 或已消费，忽略 */ });
            } catch (e) { /* 忽略 */ }
          }, function () { /* 请求失败，忽略 */ });
        }
      } catch (e) { /* 绝不因旁听逻辑影响页面请求 */ }
      return promise;
    };
  }

  function hookXhr() {
    var XHR = window.XMLHttpRequest;
    if (!XHR || !XHR.prototype) return;
    var originalOpen = XHR.prototype.open;
    var originalSend = XHR.prototype.send;

    XHR.prototype.open = function (method, url) {
      try { this.__bossIngestUrl = String(url || ""); } catch (e) { /* 忽略 */ }
      return originalOpen.apply(this, arguments);
    };

    XHR.prototype.send = function () {
      try {
        var self = this;
        var matched = matchTarget(self.__bossIngestUrl || "");
        if (matched) {
          self.addEventListener("load", function () {
            try {
              var type = self.responseType;
              var data = null;
              if (type === "" || type === "text") data = JSON.parse(self.responseText);
              else if (type === "json") data = self.response;
              if (data) emit(matched, data);
            } catch (e) { /* 非 JSON 响应，忽略 */ }
          });
        }
      } catch (e) { /* 忽略 */ }
      return originalSend.apply(this, arguments);
    };
  }

  hookFetch();
  hookXhr();
  log("hooks installed");
})();
