# 版本与发版脚本

版本号只有一处落盘位置：`internal/version/version.go` 里的 `var Version = "x.y.z"`。
所有脚本都只改这一行，避免版本号散落在多处。

但**计算基准不是这个文件，而是最近一次已发布的 git tag**：

| 情况 | 基准 base |
|---|---|
| 有 tag | 最近 tag 去掉 `v` 前缀（`v1.0.0` → `1.0.0`） |
| 无 tag（首个版本） | `0.0.0`，首个 `feat` 得到 `0.1.0` |

文件里的值只作为「当前落盘值」被覆盖，不参与递增运算，因此不会出现
「文件预置了 1.1.0、标签还停在 v1.0.0，结果算出 1.2.0、把没发布的 1.1.0 跳过」这种偏差。

| 脚本 | 作用 |
|---|---|
| `scripts/bump-version.sh` | 按规则递增版本号（可只算不写） |
| `scripts/release.sh` | 一条命令：校验 → 构建 → 提版号 → 提交 → 打标签 → 推送 |
| `scripts/install-hooks.sh` | 安装本地 git 钩子（`core.hooksPath = .githooks`） |
| `.githooks/pre-push` | 推送前跑 `gofmt` / `go vet` / `go test`，失败则阻止推送 |

---

## 1. 版本判定规则

`bump-version.sh auto` 取 `git describe --tags --abbrev=0` 得到的最近标签作为基准，
统计 `base..HEAD`（无 tag 时为全部历史）的提交，按 Conventional Commits 判定：

| 判定 | 触发条件 | 示例 |
|---|---|---|
| `major` | 提交信息含 `BREAKING CHANGE:`，或标题形如 `type!: ...` / `type(scope)!: ...` | `feat(api)!: 移除 /api/jobs 的旧字段` |
| `minor` | 标题形如 `feat: ...` / `feat(scope): ...` | `feat(deploy): 增量式部署产物` |
| `patch` | 其余提交（`fix` / `docs` / `chore` / `refactor` / `test` …） | `fix(web): 修正分页边界` |
| 不变 | 标签之后没有任何提交 | 打印 `v1.0.0 -> 1.0.0 (无变化: …)`，退出码 0，不修改文件 |

优先级：`major` > `minor` > `patch`，只要区间内出现满足条件的提交即按最高级递增。

输出左侧保留 tag 前缀（`v1.0.0`），右侧是不带前缀的目标版本，便于直接当 tag 名的后半段使用。

手动指定（`major|minor|patch`）不做提交分析，同样以 tag 为基准递增指定段
（基准 `v1.1.0` 手动 `patch` 得 `1.1.1`）。

安全阀：若推算出的目标版本**低于** `version.go` 现有值（例如基准 tag 落后于文件），
脚本报错并以退出码 1 中止，绝不把版本号写低。

### 自测示例（`git log v1.0.0..HEAD` 只有一个提交时）

```bash
# 场景 1：feat: xxx
$ git log v1.0.0..HEAD --oneline
a1b2c3d feat: 新增登录
$ ./scripts/bump-version.sh auto --dry-run
v1.0.0 -> 1.1.0 (minor: 检测到 1 个 feat 提交)

# 场景 2：fix: xxx
$ ./scripts/bump-version.sh auto --dry-run
v1.0.0 -> 1.0.1 (patch: 1 个提交中无 feat / BREAKING CHANGE)

# 场景 3：feat!: xxx（破坏性变更）
$ ./scripts/bump-version.sh auto --dry-run
v1.0.0 -> 2.0.0 (major: 检测到 1 个 BREAKING CHANGE 提交)
```

---

## 2. 手动发版流程

```bash
# 1) 一次性：安装本地钩子（让推送前自动跑 gofmt/vet/test）
./scripts/install-hooks.sh

# 2) 提交本次功能改动（普通提交，不是发版提交）
git commit -m "feat(auth): 登录与后台管理"

# 3) 一条命令发版
./scripts/release.sh
```

`release.sh` 依次执行，任一步失败立刻退出：

1. `gofmt -l .` —— 必须为空，否则打印未格式化文件并退出
2. `go vet ./...`
3. `go test ./...`
4. `go build ./...`
5. `bump-version.sh auto` —— 推算新版本号并写回 `version.go`；**打标签用的就是脚本输出的那个版本号**，
   不再从文件二次读取，避免两者不一致
6. `git add -A` → `git commit -m "chore(release): vX.Y.Z"`
   （若版本号已等于目标值、暂存区没有变化，则跳过提交，只打标签）
7. `git tag -a vX.Y.Z -m "release vX.Y.Z"`
8. `git push origin HEAD --follow-tags`（推送前会打印实际执行的 git 命令）

所有校验都在提交之前完成，因此中途失败不会留下半个发版提交。
若工作区还有未提交的改动，会被 `git add -A` 一并纳入这次发版提交——发版脚本允许这样做，
但仍建议先把功能改动单独提交，让 `chore(release):` 提交只包含版本号。

推送 `v*` 标签后，`.github/workflows/release.yml` 会自动构建 Linux 二进制并发布 Release。

---

## 3. `--dry-run` 用法

```bash
./scripts/bump-version.sh auto --dry-run
# v1.0.0 -> 1.1.0 (minor: 检测到 2 个 feat 提交)

./scripts/bump-version.sh major --dry-run
# v1.1.0 -> 2.0.0 (major: 手动指定（基准 v1.1.0）)

./scripts/release.sh --dry-run
```

- `bump-version.sh --dry-run` 只打印「base -> 目标 (判定依据)」，不写文件，可用 `git status --porcelain` 验证无改动。
- `release.sh --dry-run` 同样不改文件、不提交、不推送，但会真实执行只读校验
  （`gofmt` / `go vet` / `go test` / `go build`），确保「计划中的版本号」建立在能通过校验的代码上。
  末尾会打印将要执行的 `git` 命令供核对。
- 没有新提交时，两者都提示「没有新提交，跳过发版」并以退出码 0 结束。

`bump-version.sh` 的输出约定：主结论写到 stdout（可被脚本解析），附加提示（如 dry-run 说明）写到 stderr。

退出码：`0` 成功或无变化，`1` 环境/文件/校验失败，`2` 参数非法。

---

## 4. 为什么不在每次 push 时自动提交版本号

常见做法是让 CI 在每次 push 后自动 `bump` 并提交版本号，但本项目**刻意不做**：

1. **CI 与本地会互相打架**：CI 提交版本号后需要推回仓库，本地分支随即落后；
   开发者下一次 push 就会撞上「远端有新提交」，只能 merge 或 rebase，冲突点还是同一行版本号。
2. **版本号抖动**：一次功能开发往往 push 多次（草稿、修 CI、补测试），
   每次都自动递增会把 `1.1.0` 直接推到 `1.1.7`，semver 失去「这一版变了什么」的含义。
3. **提交信息失真**：自动提交的 `chore(release)` 会混进功能 PR 的历史里，
   `git log` 与 changelog 变噪。
4. **回去修一个坏版本很难**：自动推上去的 tag 一旦被下游拉到，撤销需要强推标签，代价高。

因此版本号只在**人主动发版**时改变，`main` 上的版本号始终等于最近一次正式发布的版本。

---

## 5. 「自动」的边界

自动的是**机械动作**：格式检查、静态检查、单测、构建、推算版本号、改文件、提交、打标签、推送——
这些全部由 `./scripts/release.sh` 一条命令完成，不需要手工改数字或手敲 5 条 git 命令。

不自动的是**决策**：这次改动要不要发版、发 `major` 还是 `minor`（`auto` 只是按提交信息给建议）、
什么时候发。**是否发版由人决定**，所以没有任何流水线会在你 push 代码时偷偷改版本号。

---

## 6. CI 与本地钩子的分工

| 位置 | 触发 | 做什么 |
|---|---|---|
| `.githooks/pre-push` | 本地 `git push` | `gofmt` / `go vet` / `go test`，失败阻止推送 |
| `.github/workflows/ci.yml` | 所有分支 push、所有 PR | 同上 + `go build` + 交叉编译 `linux/amd64`，产物上传为 artifact `jobworkspace-ci` |
| `.github/workflows/release.yml` | 推送 `v*` 标签 | 构建 `linux/amd64`、`linux/arm64`，生成校验和并发布 Release |

三者互不重叠：钩子挡住明显问题，CI 兜住「绕过钩子」和跨平台差异，release 只负责发布产物。

本地钩子可以用 `git push --no-verify` 临时绕过；CI 无法绕过，因此合入前仍会校验。
