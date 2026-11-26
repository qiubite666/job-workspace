/**
 * relay.js —— 运行在 ISOLATED world，只做一件事：
 * 把 MAIN world 通过 window.postMessage 递出来的岗位数据转成 runtime 消息发给 background。
 *
 * 之所以单独一个文件：MAIN world 里没有 chrome.runtime，必须由隔离世界的中继转发。
 */
(function () {
  "use strict";

  window.addEventListener("message", function (event) {
    if (event.source !== window) return;
    if (event.origin && event.origin !== location.origin) return;

    var data = event.data;
    if (!data || data.type !== "BOSS_INGEST" || !data.payload) return;

    var payload = data.payload;
    if (payload.platform !== "boss") return;
    if (!payload.raw || typeof payload.raw !== "object") return;

    try {
      chrome.runtime.sendMessage({ type: "BOSS_INGEST_RELAY", payload: payload }, function () {
        // 扩展被重载/更新后旧上下文会失效，这里必须消费掉 lastError，避免控制台报错。
        if (chrome.runtime.lastError) return;
      });
    } catch (e) {
      // Extension context invalidated：页面里残留的旧中继直接静默退出。
    }
  }, false);
})();
