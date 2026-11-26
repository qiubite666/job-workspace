#!/usr/bin/env bash
#
# release.sh —— 一条命令完成「校验 -> 构建 -> 提版号 -> 提交 -> 打标签 -> 推送」。
#
# 用法：
#   scripts/release.sh [--dry-run]
#
#   --dry-run  只打印计划与将要使用的版本号，不写文件、不提交、不推送
#
# 退出码：0 成功（含「没有新提交，跳过发版」）；1 任一步骤失败；2 参数非法。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$REPO_ROOT"

usage() {
  cat <<'EOF'
用法：scripts/release.sh [--dry-run]

  依次执行：gofmt 检查 -> go vet -> go test -> go build -> 提版号
            -> git add/commit -> git tag -> git push --follow-tags
  --dry-run  只打印将要发生的事，不改动任何文件、不提交、不推送
EOF
}

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "错误：未知参数 ${arg}" >&2; usage >&2; exit 2 ;;
  esac
done

echo "发版工作目录：${REPO_ROOT}"
if [ "$DRY_RUN" -eq 1 ]; then
  echo "模式：dry-run（只打印计划，不写文件、不提交、不推送）"
else
  echo "模式：实际执行"
fi

# ---------------------------------------------------------------- 1. 格式检查
echo
echo "==> 1/6 gofmt 检查（必须为空）"
echo "   \$ gofmt -l ."
fmt_out="$(gofmt -l . || true)"
if [ -n "$fmt_out" ]; then
  echo "错误：以下文件未按 gofmt 格式化，请先运行 gofmt -w ." >&2
  printf '%s\n' "$fmt_out" >&2
  exit 1
fi
echo "   通过"

# ---------------------------------------------------------------- 2. go vet
echo
echo "==> 2/6 go vet ./..."
echo "   \$ go vet ./..."
if ! go vet ./...; then
  echo "错误：go vet 未通过，已中止发版" >&2
  exit 1
fi

# ---------------------------------------------------------------- 3. go test
echo
echo "==> 3/6 go test ./..."
echo "   \$ go test ./..."
if ! go test ./...; then
  echo "错误：单元测试未通过，已中止发版" >&2
  exit 1
fi

# ---------------------------------------------------------------- 4. go build
echo
echo "==> 4/6 go build ./..."
echo "   \$ go build ./..."
if ! go build ./...; then
  echo "错误：构建失败，已中止发版" >&2
  exit 1
fi

# ---------------------------------------------------------------- 5. 提版号
echo
echo "==> 5/6 推算新版本号"
new_version=""
if [ "$DRY_RUN" -eq 1 ]; then
  bump_out="$(bash "${SCRIPT_DIR}/bump-version.sh" auto --dry-run)"
  printf '%s\n' "$bump_out"
  if printf '%s' "$bump_out" | grep -q '无变化'; then
    echo "没有新提交，跳过发版。"
    exit 0
  fi
  new_version="$(printf '%s\n' "$bump_out" | head -n1 | awk -F' -> ' '{print $2}' | awk '{print $1}')"
else
  # 以脚本输出的版本号为准，不再从文件读第二遍，避免两者不一致
  bump_out="$(bash "${SCRIPT_DIR}/bump-version.sh" auto)"
  printf '%s\n' "$bump_out"
  if printf '%s' "$bump_out" | grep -q '无变化'; then
    echo "没有新提交，跳过发版。"
    exit 0
  fi
  new_version="$(printf '%s\n' "$bump_out" | head -n1 | awk -F' -> ' '{print $2}' | awk '{print $1}')"
fi

if [ -z "$new_version" ]; then
  echo "错误：无法解析目标版本号，已中止发版" >&2
  exit 1
fi

TAG="v${new_version}"
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
  echo "错误：标签 ${TAG} 已存在，请检查版本号是否重复" >&2
  exit 1
fi

# ---------------------------------------------------------------- 6. 提交与推送
echo
echo "==> 6/6 提交、打标签并推送（${TAG}）"

quote_arg() {
  case "$1" in
    *[[:space:]]*) printf "'%s'" "$1" ;;
    *) printf '%s' "$1" ;;
  esac
}

run_git() {
  local shown="" a
  for a in "$@"; do
    shown="${shown} $(quote_arg "$a")"
  done
  printf '   $%s\n' "$shown"
  if [ "$DRY_RUN" -eq 0 ]; then
    "$@"
  fi
}

run_git git add -A
# 版本号可能已经等于目标值（例如人工预置），此时暂存区为空，跳过提交只打标签
if [ "$DRY_RUN" -eq 0 ] && git diff --cached --quiet; then
  echo "   （暂存区无改动，跳过 git commit）"
else
  run_git git commit -m "chore(release): ${TAG}"
fi
run_git git tag -a "${TAG}" -m "release ${TAG}"
run_git git push origin HEAD --follow-tags

echo
if [ "$DRY_RUN" -eq 1 ]; then
  echo "dry-run 结束：以上命令未真正执行，目标版本 ${TAG}。"
else
  echo "发版完成：${TAG} 已打标签并推送。"
fi
