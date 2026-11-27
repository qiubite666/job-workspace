/**
 * 自动补全职位的页面侧执行器（隔离世界，zhipin.com）。
 *
 * 为什么要放在页面里：详情接口需要页面自己的登录态与来源信息。
 * 由后台发起跨站请求会丢掉这些上下文，既拿不到数据也更容易被判定为异常流量。
 * 这里只负责"发一个请求、把原始 JSON 交回去"，节流与入库逻辑都在后台。
 */
"use strict";

var DETAIL_API = "/wapi/zpgeek/job/detail.json?securityId=";

chrome.runtime.onMessage.addListener(function (message, sender, sendResponse) {
  if (!message || message.type !== "BOSS_AUTOFILL_FETCH") return false;

  var sid = String(message.securityId || "").trim();
  if (!sid) {
    sendResponse({ ok: false, error: "NO_SECURITY_ID" });
    return false;
  }

  fetchDetail(sid).then(sendResponse, function (err) {
    sendResponse({ ok: false, error: String(err && err.message ? err.message : err) });
  });

  // 异步响应
  return true;
});

async function fetchDetail(securityId) {
  var response = await fetch(DETAIL_API + encodeURIComponent(securityId), {
    credentials: "include",
    headers: { Accept: "application/json" }
  });

  if (!response.ok) {
    return { ok: false, status: response.status };
  }

  var data = await response.json();
  return { ok: true, data: data };
}
