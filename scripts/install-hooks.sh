#!/usr/bin/env bash
#
# install-hooks.sh —— 把仓库内的 .githooks 目录注册为本地钩子目录。
#
# 只修改本仓库的本地 git 配置（core.hooksPath），不动全局配置、不动 CI。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$REPO_ROOT"

if [ ! -d ".githooks" ]; then
  echo "错误：找不到 .githooks 目录" >&2
  exit 1
fi

git config core.hooksPath .githooks
chmod +x .githooks/pre-push 2>/dev/null || true

echo "已安装本地 git 钩子："
echo "  core.hooksPath = $(git config --local --get core.hooksPath)"
echo "  钩子文件："
for f in .githooks/*; do
  [ -f "$f" ] || continue
  printf '    %s (%s)\n' "$f" "$( [ -x "$f" ] && echo '可执行' || echo '不可执行' )"
done
echo
echo "推送前会自动执行 gofmt / go vet / go test，任一失败则阻止推送。"
echo "（临时跳过：git push --no-verify）"
