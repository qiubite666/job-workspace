# BOSS 岗位采集桥（Chrome / Edge MV3 扩展）

在你自己已登录的浏览器里旁听 BOSS 直聘（`www.zhipin.com`）页面自身发出的岗位列表请求，把原始 JSON 上报到自建服务。扩展只取岗位数据，**不读取、不保存、不上报任何登录凭据**，登录态始终留在浏览器里。

## 文件

| 文件 | 运行环境 | 作用 |
| --- | --- | --- |
| `manifest.json` | — | MV3 清单：权限、host、注入声明 |
| `content.js` | 页面 MAIN world | hook `fetch` / `XMLHttpRequest`，命中岗位接口后 `postMessage` 抛出数据 |
| `relay.js` | 页面 ISOLATED world | 监听 `postMessage`，转成 `chrome.runtime.sendMessage` 发给后台 |
| `background.js` | service worker | 读配置、POST `/api/ingest`、更新 badge 与最近结果 |
| `popup.html` / `popup.js` | 扩展弹窗 | 配置服务端地址与 token，查看最近一次上报 |
| `README.md` | — | 本文档 |

## 安装

1. Chrome / Edge 打开 `chrome://extensions`（Edge 为 `edge://extensions`）。
2. 右上角打开「开发者模式」。
3. 点「加载已解压的扩展程序」，选择本目录 `extension/`。
4. 需要 Chrome 111+（`content_scripts` 的 `world: "MAIN"` 自 111 起支持）；建议保持浏览器最新版。

安装后点工具栏里的扩展图标：

- **服务端地址**：`https://job.qiubite.fun`，本地调试可填 `http://127.0.0.1:8787`。
- **X-Ingest-Token**：与服务端配置的 token 一致。服务端首次启动会在 `<data>/ingest.token` 生成（0600），把文件内容原样粘贴进来即可。
- 点「保存」。想确认服务是否可达，可点「测试连接」（请求 `GET {server}/api/health`）。

然后打开 BOSS 直聘的搜索页或推荐页并正常滚动，页面每发出一页岗位请求，扩展就会上报一次。

## 工作方式

```
BOSS 页面 fetch/XHR
        │  响应被 clone / load 事件旁听（请求本身不修改、不重发）
        ▼
content.js（MAIN world）  命中 /wapi/zpgeek/... 且 zpData 非空
        │  window.postMessage({type:"BOSS_INGEST", payload})
        ▼
relay.js（ISOLATED world）  chrome.runtime.sendMessage
        ▼
background.js  POST {server}/api/ingest   Header: X-Ingest-Token
        ▼
badge：成功显示 added+updated（绿色，3 秒后清除）；失败显示 x（红色）
popup：保留最近一次结果或错误文本
```

命中的接口：

- `/wapi/zpgeek/search/joblist.json`
- `/wapi/zpgeek/pc/recommend/job/list.json`
- `/wapi/zpgeek/recommend/job/list.json`

请求体严格按契约：

```json
{
  "platform": "boss",
  "query": "运维",
  "city": "呼和浩特",
  "raw": { "zpData": { "...": "..." } }
}
```

`query` / `city` 为可选字段：优先取页面 URL 的 `query` 参数与城市控件文本，取不到就留空，由服务端自行决定如何处理；不会把 BOSS 的数字城市码当作城市名上报。

单批请求体上限 4MB（与服务端 `ingestBodyLimit` 一致），超出则本地直接跳过并提示。响应统计字段两种口径都兼容：`added/updated/duplicates/total` 或 `received/mapped/dropped/added/total`，弹窗只展示实际存在的字段。

## MAIN world 注入方式

采用 **`content_scripts` + `"world": "MAIN"`（声明式）**，而不是 `chrome.scripting.registerContentScripts`，原因：

1. 声明式在 `document_start` 就注入，能赶在页面首个岗位请求之前完成 hook。`registerContentScripts` 需要等 service worker 启动后再注册，且对已打开的标签页必须再调 `executeScript` 补注入，容易漏掉首屏请求。
2. 清单里一眼可见注入了什么、注入到哪个世界，审计成本低；不需要动态注册的 `persistAcrossSessions` 等额外参数。
3. 代价是需要 Chrome 111+，本项目使用场景可控，已在 `minimum_chrome_version` 里写明。

## 节流与错误处理

- **节流**：在 `content.js` 里以 `pathname + search` 为键，2 秒内同一 URL 只处理一次；键表超过 40 条时清理过期项。无限滚动翻页参数不同，仍会正常逐页上报。
- **空包拦截**：`zpData` 缺失、为 `{}` 或空数组时不上报。
- **配置缺失**：badge 显示 `!`（琥珀色），不上报，popup 记录「未配置服务端地址或 token」。
- **接口异常**：401 → badge `x`，错误文本「鉴权失败（401）：token 不正确或已失效」；其他非 2xx、网络错误、15 秒超时同理，均写入 `lastResult`。
- **成功**：badge 显示 `added + updated`，绿色，3 秒后清空。
- 所有 `chrome.runtime.sendMessage` 回调都检查 `chrome.runtime.lastError`，扩展重载后旧页面不会刷控制台报错。
- 上报使用 `credentials: "omit"`，即使目标服务同域也不会带上浏览器凭据。

## 本地验证（127.0.0.1 模拟服务端）

1. 新建临时文件 `mock_ingest.py`：

   ```python
   import json
   from http.server import BaseHTTPRequestHandler, HTTPServer

   TOKEN = "dev-token"

   class H(BaseHTTPRequestHandler):
       def _json(self, code, obj):
           body = json.dumps(obj).encode()
           self.send_response(code)
           self.send_header("Content-Type", "application/json")
           self.send_header("Content-Length", str(len(body)))
           self.end_headers()
           self.wfile.write(body)

       def do_GET(self):
           if self.path == "/api/health":
               self._json(200, {"ok": True, "version": "mock-1"})
           else:
               self._json(404, {"ok": False})

       def do_POST(self):
           if self.path != "/api/ingest":
               self._json(404, {"ok": False})
               return
           if self.headers.get("X-Ingest-Token") != TOKEN:
               self._json(401, {"ok": False, "error": "UNAUTHORIZED"})
               return
           size = int(self.headers.get("Content-Length") or 0)
           data = json.loads(self.rfile.read(size) or b"{}")
           zp = (data.get("raw") or {}).get("zpData") or {}
           jobs = zp.get("jobList") or []
           print("ingest", data.get("platform"), data.get("query"), data.get("city"), "jobs=", len(jobs))
           self._json(200, {"ok": True, "added": len(jobs), "updated": 0,
                            "duplicates": 0, "total": len(jobs)})

       def log_message(self, *args):
           pass

   HTTPServer(("127.0.0.1", 8787), H).serve_forever()
   ```

2. 运行 `python mock_ingest.py`（控制台会打印每次上报的 platform / query / city / 岗位数）。
3. 扩展弹窗填 `http://127.0.0.1:8787`、token `dev-token`，保存，点「测试连接」应显示 `连接成功：HTTP 200（version mock-1）`。
4. 打开 `https://www.zhipin.com/web/geek/job?query=运维&city=101080100`，滚动列表。
5. 预期：终端出现 `ingest boss 运维 ... jobs=N`；图标 badge 显示本轮新增/更新数并在 3 秒后消失；弹窗「最近一次上报」显示新增/更新/重复/总数。
6. 把 token 改错再触发一次：badge 变红显示 `x`，弹窗显示 401 错误文本；改回正确 token 后再触发即可恢复。

命令行自查契约（不依赖浏览器）：

```bash
curl -s -X POST http://127.0.0.1:8787/api/ingest \
  -H "Content-Type: application/json" \
  -H "X-Ingest-Token: dev-token" \
  -d '{"platform":"boss","query":"t","city":"c","raw":{"zpData":{"jobList":[{"a":1}]}}}'
# {"ok": true, "added": 1, "updated": 0, "duplicates": 0, "total": 1}
```

## 隐私与边界

- 不读取浏览历史、不注入任何可见 UI、不引第三方脚本。
- 只上报岗位接口的响应 JSON，以及页面上的搜索词、城市名。
- 服务端地址只允许 `http(s)`，配置保存在 `chrome.storage.local`。

## 排查

- **badge 一直是 `!`**：弹窗里地址或 token 未保存成功。
- **badge `x` 且弹窗提示网络错误**：自建服务是否启动、地址是否可达；若换成非清单内域名，需要把该域名加到 `manifest.json` 的 `host_permissions` 后重新加载扩展。
- **没有任何上报**：确认页面确实加载了岗位列表（滚动触发翻页）；打开扩展的 service worker 控制台（`chrome://extensions` → 该扩展 → `service worker`）查看是否有报错；必要时把 `content.js` 里的 `DEBUG` 改成 `true` 重新加载，观察 `[boss-ingest]` 日志。
- **改代码后不生效**：在 `chrome://extensions` 点该扩展的刷新按钮，并刷新 BOSS 页面（MAIN world 脚本需要整页重载）。
