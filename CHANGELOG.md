# 变更日志

按「修复 / 新增 / 优化」分类记录。自动化版本从 Conventional Commits 生成，
详见 `scripts/release-notes.sh`；本文用于补齐未成功创建 Release 的版本段。

---

## v2.0.2（2026-11-26）

### 修复
- 让发布工作流的冒烟测试兼容 `--help` 的非零退出码：`collect` 使用 `flag.ContinueOnError`，
  `--help` 返回退出码 1，在 `pipefail` 下导致管道失败；改为先写变量再匹配 (4a40b1d)

## v2.0.1（2026-11-26）

### 修复
- 修复发布工作流冒烟测试并消除 Node.js 20 弃用警告：合并 stderr、修正单横线参数名匹配，
  并把 `actions/checkout` / `actions/setup-go` 升级到 v7 (8b66000)

## v2.0.0（2026-11-26）

**不兼容变更：采集只剩 live 模式，`sample` / `local` 与 `--mode` / `--file` 已移除。**

### 新增
- 全站登录门禁：首页与 `/admin` 共用会话，admin 登录后直接进入管理后台；
  岗位、采集、后台接口全部要求登录，nginx Basic Auth 同步移除 (51e4e6e)
- 收藏 / 投递状态 / 备注云端化：按账号隔离存储于 `data/userstate/<username>.json`，
  新增 `GET/PUT /api/me/state`，前端 800ms 防抖自动保存 + 立即保存 + 状态提示；
  服务端归一化阶段白名单与备注长度，拒绝路径穿越 (51e4e6e)
- 分类变更日志生成器 `scripts/release-notes.sh`，按新增/修复/优化分组并附短 SHA (2f6d10b)
- Windows amd64 二进制纳入 Release，ci.yml 同步增加 Windows 构建 (2f6d10b)

### 修复
- 采集失败时若子进程 stderr 为空，回退用 stdout 的 JSON 报告作为错误详情，
  前端不再只看到 "exit status 2" (51e4e6e)
- 无登录态时提前返回 `AUTH_REQUIRED`，不再发出注定被风控的请求 (d14b917)

### 优化
- 采集收敛为 live 模式：删除 sample/local 分支、示例数据回退与相关文件，
  `collect --help` 不再出现模式参数；robots 默认拦截策略不变 (d14b917)
- 部署配置去掉 Basic Auth 与 htpasswd 生成逻辑，「只增不改」预检保持完整 (2f6d10b)
- README 补充 Windows 使用方式与全站登录章节；V1 契约标注为历史留档 (3458b34)
