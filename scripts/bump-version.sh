#!/usr/bin/env bash
#
# bump-version.sh —— 推算下一个 semver 版本号，并写回 internal/version/version.go。
#
# 用法：
#   scripts/bump-version.sh [major|minor|patch|auto] [--dry-run]
#
# 版本基准是「最近一次已发布的 tag」，不是文件里现有的值：
#   有 tag  -> base = 最近 tag 去掉 v 前缀（如 v1.0.0 -> 1.0.0）
#   无 tag  -> base = 0.0.0（首个版本，例如首个 feat 得到 0.1.0）
#   文件里的版本只作为「当前落盘值」被覆盖，不参与计算。
#
#   auto  按 base..HEAD 的提交信息（Conventional Commits）判定递增位
#   --dry-run  只打印「base -> 目标 与判定依据」，不修改任何文件
#
# 退出码：0 成功（含「无变化」）；1 环境或文件异常；2 参数非法。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${REPO_ROOT}/internal/version/version.go"

usage() {
  cat <<'EOF'
用法：scripts/bump-version.sh [major|minor|patch|auto] [--dry-run]

  major      1.2.3 -> 2.0.0
  minor      1.2.3 -> 1.3.0
  patch      1.2.3 -> 1.2.4
  auto       按最近 tag 之后的提交信息自动判定（默认）
  --dry-run  只打印判定结果，不修改文件

基准版本取最近一次 git tag（无 tag 时为 0.0.0），文件里的版本号不参与计算。
EOF
}

MODE=""
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    major|minor|patch|auto)
      if [ -n "$MODE" ]; then
        echo "错误：版本类型只能指定一次（已指定 ${MODE}，又收到 ${arg}）" >&2
        exit 2
      fi
      MODE="$arg"
      ;;
    --dry-run)
      DRY_RUN=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "错误：未知参数 ${arg}" >&2
      usage >&2
      exit 2
      ;;
  esac
done
MODE="${MODE:-auto}"

if [ ! -f "$VERSION_FILE" ]; then
  echo "错误：找不到版本文件 ${VERSION_FILE}" >&2
  exit 1
fi

# 文件当前值：只用于写回比较与提示，不参与版本计算
current="$(sed -nE 's/^var Version = "([0-9]+\.[0-9]+\.[0-9]+)".*/\1/p' "$VERSION_FILE" | head -n1)"
if [ -z "$current" ]; then
  echo "错误：${VERSION_FILE} 中未找到形如 var Version = \"x.y.z\" 的行" >&2
  exit 1
fi

# ---------------------------------------------------------------- 基准版本
last_tag="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null || true)"
if [ -n "$last_tag" ]; then
  base="${last_tag#v}"
  display_base="$last_tag"
else
  base="0.0.0"
  display_base="0.0.0"
fi

if ! printf '%s' "$base" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "错误：最近标签 ${last_tag} 不是 semver 形式（期望 vX.Y.Z），无法自动推算基准版本" >&2
  exit 1
fi

IFS='.' read -r BASE_MAJOR BASE_MINOR BASE_PATCH <<< "$base"

bump() {
  case "$1" in
    major) printf '%d.0.0' "$((BASE_MAJOR + 1))" ;;
    minor) printf '%d.%d.0' "$BASE_MAJOR" "$((BASE_MINOR + 1))" ;;
    patch) printf '%d.%d.%d' "$BASE_MAJOR" "$BASE_MINOR" "$((BASE_PATCH + 1))" ;;
  esac
}

# 提交区间：有 tag 用 tag..HEAD，否则用全部历史
if [ -n "$last_tag" ]; then
  range="${last_tag}..HEAD"
  range_desc="${last_tag} 之后"
else
  range="HEAD"
  range_desc="全部历史"
fi

total="$(git -C "$REPO_ROOT" rev-list --count "$range" 2>/dev/null || echo 0)"
total="${total:-0}"

# ---------------------------------------------------------------- 判定
target_mode=""
reason=""

if [ "$total" -eq 0 ] && [ "$MODE" = "auto" ]; then
  echo "${display_base} -> ${base} (无变化: ${range_desc}没有可发布的提交)"
  exit 0
fi

if [ "$MODE" = "auto" ]; then
  # 多个 --grep 之间是「或」；-E 让正则支持 (...) 分组
  breaking_count="$(git -C "$REPO_ROOT" rev-list --count -E \
    --grep='BREAKING[ -]CHANGE:' \
    --grep='^[A-Za-z][A-Za-z0-9_-]*(\([^)]*\))?!:' \
    "$range" 2>/dev/null || echo 0)"
  feat_count="$(git -C "$REPO_ROOT" rev-list --count -E \
    --grep='^feat(\([^)]*\))?:' \
    "$range" 2>/dev/null || echo 0)"
  breaking_count="${breaking_count:-0}"
  feat_count="${feat_count:-0}"

  if [ "$breaking_count" -gt 0 ]; then
    target_mode="major"
    reason="major: 检测到 ${breaking_count} 个 BREAKING CHANGE 提交"
  elif [ "$feat_count" -gt 0 ]; then
    target_mode="minor"
    reason="minor: 检测到 ${feat_count} 个 feat 提交"
  else
    target_mode="patch"
    reason="patch: ${total} 个提交中无 feat / BREAKING CHANGE"
  fi
else
  target_mode="$MODE"
  reason="${MODE}: 手动指定（基准 ${display_base}）"
fi

target="$(bump "$target_mode")"

# 安全阀：不允许把版本号写低（例如手工指定 patch 时基准 tag 落后于文件值）
if [ "$target" != "$current" ] &&
  [ "$(printf '%s\n%s\n' "$target" "$current" | sort -V | head -n1)" = "$target" ]; then
  echo "错误：目标版本 ${target} 低于 version.go 当前值 ${current}，已中止" >&2
  echo "      基准为最近标签 ${display_base}；如需强制回退，请手工编辑 ${VERSION_FILE}" >&2
  exit 1
fi

# 输出统一格式：base -> 目标 (判定依据)，左侧保留 tag 前缀便于人读
echo "${display_base} -> ${target} (${reason})"

# 文件落盘值与基准不一致时给个提示（不改变结果）
if [ "$current" != "$base" ]; then
  echo "（提示：version.go 当前写的是 ${current}，与最近标签基准 ${base} 不一致，本次以标签为准）" >&2
fi

if [ "$DRY_RUN" -eq 1 ]; then
  echo "（dry-run：未修改任何文件）" >&2
  exit 0
fi

if [ "$current" = "$target" ]; then
  exit 0
fi

# 只替换版本号那一行，保持原有引号与赋值格式
sed -i -E "s/^var Version = \"[0-9]+\.[0-9]+\.[0-9]+\"/var Version = \"${target}\"/" "$VERSION_FILE"
