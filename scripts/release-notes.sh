#!/usr/bin/env bash
#
# release-notes.sh —— 按 Conventional Commits 生成分类变更日志（markdown）。
#
# 用法：
#   scripts/release-notes.sh [--tag <tag>] [--dry-run] [-h|--help]
#
#   --tag <tag>  以 <tag> 为区间终点（默认 HEAD）。标签已存在时，
#                区间取「该标签的上一个 tag .. 该标签」；标签尚未创建时退回 HEAD。
#   --dry-run    结果打印到标准输出，并在标准错误附加一行区间说明，不写任何文件。
#
# 分类规则：
#   feat                                    -> ## 新增
#   fix                                     -> ## 修复
#   perf/refactor/style/docs/test/build/
#   ci/chore                                -> ## 优化
#   无 Conventional Commits 前缀的提交      -> ## 其他（仅在该分类非空时输出）
#   提交含 type! 或 BREAKING CHANGE 页脚    -> 条目末尾追加 **（不兼容变更）**
#
# 区间：最近 tag..HEAD；仓库无 tag 时取全部历史（最多 50 条）。
# 每条格式：- <去掉 type 前缀的描述> (<7 位短 SHA>)，分类内按提交时间倒序。
# 空分类不输出小节；全部为空时输出「- 本次发布无用户可见变更」。
#
# 退出码：0 成功；1 环境异常；2 参数非法。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$REPO_ROOT"

REPO_SLUG="qiubite666/job-workspace"

usage() {
  cat <<'EOF'
用法：scripts/release-notes.sh [--tag <tag>] [--dry-run]

  按 Conventional Commits 把提交分成「新增 / 修复 / 优化 / 其他」四类，
  输出可直接作为 GitHub Release 正文的 markdown。

  --tag <tag>  区间终点（默认 HEAD）；标签已存在时自动取上一个 tag 作为起点
  --dry-run    只打印到标准输出，不写文件
  -h, --help   显示本帮助

区间默认为「最近 tag..HEAD」；仓库无 tag 时取全部历史（最多 50 条）。
EOF
}

TAG_ARG=""
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tag)
      [ $# -ge 2 ] || { echo "错误：--tag 需要一个标签名" >&2; exit 2; }
      TAG_ARG="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "错误：未知参数 $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

git rev-parse --git-dir >/dev/null 2>&1 || {
  echo "错误：当前目录不是 git 仓库（${REPO_ROOT}）" >&2
  exit 1
}

# ---------------------------------------------------------------- 区间判定
REV="HEAD"
TAG_LABEL="HEAD"
PREV_TAG=""
LOG_ARGS=()

if [ -n "$TAG_ARG" ]; then
  TAG_LABEL="$TAG_ARG"
  if git rev-parse -q --verify "refs/tags/${TAG_ARG}" >/dev/null 2>&1; then
    REV="${TAG_ARG}"
    # 以该标签的上一个 tag 为起点，否则区间为空
    PREV_TAG="$(git describe --tags --abbrev=0 "${TAG_ARG}^" 2>/dev/null || true)"
  else
    # 标签尚未创建（本地预演）：以 HEAD 为准
    PREV_TAG="$(git describe --tags --abbrev=0 HEAD 2>/dev/null || true)"
  fi
else
  PREV_TAG="$(git describe --tags --abbrev=0 HEAD 2>/dev/null || true)"
fi

if [ -n "$PREV_TAG" ]; then
  RANGE="${PREV_TAG}..${REV}"
  RANGE_DESC="${PREV_TAG}..${TAG_LABEL}"
else
  # 无 tag：取全部历史，最多 50 条
  RANGE="${REV}"
  RANGE_DESC="全部历史（最多 50 条）"
  LOG_ARGS=(-n 50)
fi

# ---------------------------------------------------------------- 分类收集
declare -a FEAT=() FIX=() OPT=() OTHER=()

add_item() {
  local bucket="$1" desc="$2" short="$3" breaking="$4"
  local line="- ${desc} (${short})"
  if [ "$breaking" -eq 1 ]; then
    line="${line} **（不兼容变更）**"
  fi
  case "$bucket" in
    feat) FEAT+=("$line") ;;
    fix) FIX+=("$line") ;;
    opt) OPT+=("$line") ;;
    other) OTHER+=("$line") ;;
  esac
}

# \x1f 分隔字段、\x1e 分隔记录，避免正文里的换行破坏解析
while IFS= read -r -d $'\x1e' record; do
  record="${record#$'\n'}"
  [ -n "$record" ] || continue

  short="${record%%$'\x1f'*}"
  rest="${record#*$'\x1f'}"
  subject="${rest%%$'\x1f'*}"
  body="${rest#*$'\x1f'}"

  [ -n "$subject" ] || continue

  breaking=0
  if printf '%s' "$body" | grep -qE 'BREAKING[ -]CHANGE'; then
    breaking=1
  fi
  if printf '%s' "$subject" | grep -qE '^[A-Za-z][A-Za-z0-9_-]*(\([^)]*\))?!:'; then
    breaking=1
  fi

  if printf '%s' "$subject" | grep -qE '^[A-Za-z][A-Za-z0-9_-]*(\([^)]*\))?!?:[[:space:]]*'; then
    type="$(printf '%s' "$subject" | sed -E 's/^([A-Za-z][A-Za-z0-9_-]*).*/\1/' | tr '[:upper:]' '[:lower:]')"
    scope="$(printf '%s' "$subject" | sed -nE 's/^[A-Za-z][A-Za-z0-9_-]*\(([^)]*)\).*/\1/p')"
    desc="$(printf '%s' "$subject" | sed -E 's/^[A-Za-z][A-Za-z0-9_-]*(\([^)]*\))?!?:[[:space:]]*//')"
    # 版本号提版提交（chore(release): vX.Y.Z）不构成用户可见变更，跳过
    if [ "$type" = "chore" ] && [ "$scope" = "release" ]; then
      continue
    fi
    case "$type" in
      feat) add_item feat "$desc" "$short" "$breaking" ;;
      fix) add_item fix "$desc" "$short" "$breaking" ;;
      perf|refactor|style|docs|test|build|ci|chore)
        add_item opt "$desc" "$short" "$breaking" ;;
      *) add_item other "$subject" "$short" "$breaking" ;;
    esac
  else
    # 非 Conventional Commits：原样进「其他」
    add_item other "$subject" "$short" "$breaking"
  fi
done < <(git log "${LOG_ARGS[@]+"${LOG_ARGS[@]}"}" --format='%h%x1f%s%x1f%b%x1e' "$RANGE")

# ---------------------------------------------------------------- 输出
NOTES=""

section() {
  local title="$1"
  shift
  [ "$#" -gt 0 ] || return 0
  NOTES="${NOTES}${title}"$'\n'
  local item
  for item in "$@"; do
    NOTES="${NOTES}${item}"$'\n'
  done
}

section "## 新增" ${FEAT[@]+"${FEAT[@]}"}
section "## 修复" ${FIX[@]+"${FIX[@]}"}
section "## 优化" ${OPT[@]+"${OPT[@]}"}
section "## 其他" ${OTHER[@]+"${OTHER[@]}"}

if [ -z "$NOTES" ]; then
  NOTES="- 本次发布无用户可见变更"$'\n'
fi

if [ -n "$PREV_TAG" ]; then
  NOTES="${NOTES}"$'\n'"**Full Changelog**: https://github.com/${REPO_SLUG}/compare/${PREV_TAG}...${TAG_LABEL}"$'\n'
else
  NOTES="${NOTES}"$'\n'"**Full Changelog**: https://github.com/${REPO_SLUG}/commits/${TAG_LABEL}"$'\n'
fi

printf '%s' "$NOTES"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "（dry-run：区间 ${RANGE_DESC}；未写入任何文件）" >&2
fi
