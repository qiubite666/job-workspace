# 部署说明（Linux · systemd · nginx）

本文档对应「**服务器只能新增文件，不得删除/覆盖/修改任何既有文件**」这一硬约束。
所有落位路径、冲突处理方式、回滚手段都围绕这条约束设计。

---

## 1. 落位清单

| 类型 | 目标路径 | 是否新增 |
|---|---|---|
| 二进制 | `/home/ops/service/job-workspace/jobworkspace` | 新增（0755，属主 `ops`） |
| 运行数据 | `/home/ops/service/job-workspace/data/` | 新增目录（`mkdir -p`，**不清理**其中既有文件） |
| 可选环境变量 | `/home/ops/service/job-workspace/env` | 可选新增（0600） |
| systemd 单元 | `/etc/systemd/system/job-workspace.service` | 新增 |
| nginx 站点 | `/etc/nginx/sites-enabled/job-workspace.conf` | 新增 |
| Basic Auth | `/etc/nginx/job-workspace.htpasswd` | 新增（0640，`root:www-data`） |
| 证书 | `/etc/nginx/cert/qiubite.fun.crt` `/etc/nginx/cert/qiubite.fun.key` | **只读引用，不改动** |

---

## 2. 同名冲突处理矩阵

| 目标 | 已存在时 | 处理方式 |
|---|---|---|
| 二进制 | 是 | **中止**。改用 `jobworkspace-<日期>`，并在**新建**的 systemd 单元里引用（旧单元不动） |
| nginx 站点 | 是 | **中止**。加 `--conf-suffix <日期>` 生成 `job-workspace-<日期>.conf`（nginx 会一并加载） |
| systemd 单元 | 是 | **中止**。新建 `job-workspace-v2.service` + 独立端口（两个单元不能抢同一端口） |
| htpasswd | 是 | **不覆盖**，保留既有凭据并告警（旧密码继续有效） |
| `data/` 目录 | 是 | 复用，脚本不做任何清理 |
| 证书 | 是 | 只读；只做 SAN 覆盖校验，绝不重写 |

额外两条硬性预检（任一不过，脚本在改动任何文件前退出）：

1. **`server_name` 已被占用** → 中止。nginx 对重复 `server_name` 只保留先加载的那个，
   新站点会被静默忽略，属于典型的「部署成功但页面不对」。
2. **证书 SAN 未覆盖该域名** → 中止。否则 TLS 握手直接失败。

---

## 3. 为什么用子域名 `job.qiubite.fun`

`qiubite.fun` 这个 apex 域名已有站点在跑。要复用同一 `server_name`，只能往既有 server 块里
**加 `location`**——那是修改现有文件，与约束冲突。因此新增独立子域名与独立 server 块，
是「只增不改」前提下唯一自洽的方案。

如果证书只签了 apex、不含 `*.qiubite.fun`，脚本会直接拦住并给出两条出路：
换证书，或换一个已被覆盖的域名。**不要**为了绕过去而改既有配置。

---

## 4. 部署步骤

```bash
# 0) 本地构建（Linux 版单二进制）
./deploy/build-linux.sh v1.0.0
# 产出 dist/jobworkspace-linux-amd64（含 arm64）与 sha256sums.txt

# 1) 上传（只传新文件，不覆盖服务器上任何东西）
scp dist/jobworkspace-linux-amd64 ops@<host>:/tmp/jobworkspace-linux-amd64

# 2) 干跑：只做只读预检，打印将要新增的路径，不做任何改动
ssh ops@<host> 'sudo /tmp/install-additive.sh --binary /tmp/jobworkspace-linux-amd64 --dry-run'

# 3) 正式执行
ssh ops@<host> 'sudo /tmp/install-additive.sh --binary /tmp/jobworkspace-linux-amd64'
```

脚本执行顺序：预检 → 建目录 → 装二进制 → 装 systemd 单元并启动 → 装 nginx 站点 →
`nginx -t` → `nginx -s reload` → 验收。

若 `nginx -t` 失败，脚本会**撤回自己刚新增的站点配置**并退出，既有配置不受影响。

### 常用开关

| 参数 | 作用 |
|---|---|
| `--dry-run` | 只预检，不改动 |
| `--server-name job2.qiubite.fun` | 换域名（需证书覆盖） |
| `--conf-suffix 20261126` | 站点配置改名，避开同名冲突 |
| `--user www` | 换运行用户（默认 `ops`） |
| `--no-auth` | 关闭 Basic Auth（**公网裸奔，不建议**） |
| `--skip-nginx` / `--skip-systemd` | 只做其中一部分 |

---

## 5. 验收

```bash
systemctl status job-workspace --no-pager
curl -fsS http://127.0.0.1:8787/api/health
curl -kI https://job.qiubite.fun/            # 期望 401（鉴权生效）或 200
journalctl -u job-workspace -n 50 --no-pager
```

浏览器打开 `https://job.qiubite.fun/`，用脚本输出的用户名/密码登录。

---

## 6. 回滚

约束说的是「不得删除/覆盖**既有**文件」，本脚本新增的文件此前并不存在，删除它们不违反约束：

```bash
systemctl disable --now job-workspace
rm -f /etc/systemd/system/job-workspace.service /etc/nginx/sites-enabled/job-workspace.conf
systemctl daemon-reload && nginx -t && nginx -s reload
# 应用目录按需保留（内含 data/jobs.json）
```

`--no-auth` 场景下脚本会给站点配置打临时 `.bak` 再删除，同样只作用于自己刚写的文件。

---

## 7. 关于「只增不改」与后续升级

本次是首次部署，所有文件都不存在，天然满足约束。但**升级必然会覆盖二进制与单元文件**，
这一步无法在「不覆盖」前提下完成。升级时需要你显式授权其中之一：

- 允许覆盖 `/home/ops/service/job-workspace/jobworkspace`（最省事，推荐）；
- 或保留旧二进制，新建 `job-workspace-v2.service` + 新端口，nginx 切 upstream——但切 upstream 仍需改 nginx 配置。

建议：首次部署后就与运维约定「应用目录内允许原地替换」，把约束收敛到 `/etc` 下的配置。

---

## 8. 安全须知

- 服务只监听 `127.0.0.1:8787`，对外仅经 nginx。
- 工作台**没有账号体系**，默认整站 Basic Auth。关闭鉴权等于把「岗位库 + 采集接口」公开，
  其中 `/api/collect` 会真实发起对招聘平台的请求，被滥用会牵连服务器 IP。
- 若只想自己用，更稳妥的做法是把 `auth_basic` 换成 IP 白名单：
  ```nginx
  allow 1.2.3.4;
  deny  all;
  ```
  （替换掉 `auth_basic` 两行即可，仍只改本文件。）
- `BOSS_COOKIE` 属于账号凭证，放在 `env`（0600），不要写进单元文件或仓库。
