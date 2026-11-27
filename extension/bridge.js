/**
 * bridge.js —— 让工作台网页（job.qiubite.fun）能触发扩展的批量补全。
 *
 * 为什么需要它：扩展弹窗不是网页，外部无法点击；把触发入口放到工作台上，
 * 用户（以及自动化验收）就能从页面直接发起，并看到实时进度。
 *
 * 安全边界：只接受同源、同窗口、且 source 标记正确的消息；
 * 只暴露"开始补全"这一个动作，不提供读取令牌等能力。
 */
"use strict";

var PAGE_TAG = "wb-bridge";

window.addEventListener("message", function (event) {
  if (event.source !== window) return;
  if (event.origin && event.origin !== location.origin) return;

  var data = event.data;
  if (!data || data.source !== PAGE_TAG) return;

  if (data.type === "AUTOFILL_START") {
    chrome.runtime.sendMessage({ type: "BOSS_AUTOFILL_START", limit: data.limit }, function (res) {
      var payload = {
        source: PAGE_TAG,
        type: "AUTOFILL_ACK",
        ok: !!(res && res.ok),
        error: (res && res.error) || "",
        total: (res && res.total) || 0
      };
      try {
        window.postMessage(payload, location.origin);
      } catch (e) { /* 页面已卸载 */ }
    });
    return;
  }

  if (data.type === "AUTOFILL_STATUS") {
    chrome.storage.local.get(["autofillState"], function (cfg) {
      postState(cfg && cfg.autofillState);
    });
  }
}, false);

/* 进度变化主动推给页面，避免页面轮询 */
chrome.storage.onChanged.addListener(function (changes, area) {
  if (area !== "local" || !changes.autofillState) return;
  postState(changes.autofillState.newValue);
});

function postState(state) {
  try {
    window.postMessage({ source: PAGE_TAG, type: "AUTOFILL_STATE", state: state || null }, location.origin);
  } catch (e) { /* 忽略 */ }
}

/* 页面加载后先同步一次当前状态 */
chrome.storage.local.get(["autofillState"], function (cfg) {
  postState(cfg && cfg.autofillState);
});
