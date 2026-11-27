/**
 * 批量补全的页面侧执行器（隔离世界，zhipin.com）。
 *
 * 背景：直接访问 /job_detail/<id>.html 不会触发详情请求（被判定为非常规访问），
 * 直接调接口又缺参数。所以改用"重放"：拿用户手动浏览时抓到的真实请求地址
 * 作为模板，只替换 securityId，其余参数、来源、登录态都保持原样。
 */
"use strict";

chrome.runtime.onMessage.addListener(function (message, sender, sendResponse) {
  if (!message || message.type !== "BOSS_AUTOFILL_FETCH") return false;

  var template = String(message.template || "").trim();
  var sid = String(message.securityId || "").trim();
  if (!template || !sid) {
    sendResponse({ ok: false, error: "BAD_ARGS" });
    return false;
  }

  var url;
  try {
    url = buildUrl(template, sid);
  } catch (err) {
    sendResponse({ ok: false, error: "BAD_TEMPLATE" });
    return false;
  }

  fetchDetail(url).then(sendResponse, function (err) {
    sendResponse({ ok: false, error: String(err && err.message ? err.message : err) });
  });

  return true;
});

/** 用模板地址生成目标请求：只改 securityId，其余参数原样保留。 */
function buildUrl(template, securityId) {
  var u = new URL(template, location.origin);
  u.searchParams.set("securityId", securityId);
  return u.toString();
}

async function fetchDetail(url) {
  var response = await fetch(url, {
    credentials: "include",
    headers: { Accept: "application/json" }
  });

  if (!response.ok) {
    return { ok: false, status: response.status };
  }

  var data = await response.json();
  return { ok: true, data: data };
}
