#!/usr/bin/env bash
#
# job-workspace 增量部署脚本（严格「只新增、不覆盖」）
#
# 设计约束：
#   1. 只创建当前不存在的文件；目标已存在时**一律中止**并给出替代方案，绝不覆盖。
#   2. 不改动任何既有 nginx / systemd / 证书文件，只读取与引用。
#   3. 唯一允许的「写」是创建本脚本自己产生的文件，以及启用服务时 systemd 生成的软链。
#
# 用法：
#   ./install-additive.sh --binary ./dist/jobworkspace-linux-amd64 [--dry-run]
#   ./install-additive.sh --binary ... --server-name job2.qiubite.fun --conf-suffix 20261126
#   ./install-additive.sh --binary ... --no-auth      # 不加 Basic Auth（公网裸奔，慎用）
#
set -euo pipefail

APP_DIR="/home/ops/service/job-workspace"
BIN_NAME="jobworkspace"
SERVER_NAME="job.qiubite.fun"
CONF_NAME="job-workspace.conf"
UNIT_NAME="job-workspace.service"
AUTH_FILE="/etc/nginx/job-workspace.htpasswd"
SVC_USER="ops"
LISTEN_PORT="8787"

BINARY=""
DRY_RUN=0
USE_AUTH=1
DO_NGINX=1
DO_SYSTEMD=1

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ok\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  warn\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m  冲突/中止\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)      BINARY="${2:-}"; shift 2 ;;
    --server-name) SERVER_NAME="${2:-}"; shift 2 ;;
    --conf-suffix) CONF_NAME="job-workspace-${2:-}.conf"; shift 2 ;;
    --user)        SVC_USER="${2:-}"; shift 2 ;;
    --no-auth)     USE_AUTH=0; shift ;;
    --skip-nginx)  DO_NGINX=0; shift ;;
    --skip-systemd) DO_SYSTEMD=0; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

[ -n "$BINARY" ] || die "必须用 --binary 指定 Linux 版二进制路径"
[ -f "$BINARY" ] || die "二进制不存在：$BINARY"

CONF_PATH="/etc/nginx/sites-enabled/${CONF_NAME}"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"
BIN_PATH="${APP_DIR}/${BIN_NAME}"

# ------------------------------------------------------------------
# 1. 只读预检：任何一项不过都在「尚未改动任何文件」时退出
# ------------------------------------------------------------------
log "预检（只读）"

[ "$(id -u)" -eq 0 ] || die "需要 root 执行"
ok "root 权限"

id "$SVC_USER" >/dev/null 2>&1 || die "运行用户 $SVC_USER 不存在；请用 --user 指定，或先创建该用户"
ok "运行用户 $SVC_USER 存在"

for p in "$BIN_PATH" "$CONF_PATH" "$UNIT_PATH"; do
  if [ -e "$p" ]; then
    die "目标已存在：$p
       处理方式（不覆盖）：
       - 二进制：改用 ${BIN_NAME}-$(date +%Y%m%d) 并在新的 systemd 单元里引用；
       - nginx 配置：加 --conf-suffix $(date +%Y%m%d) 生成 ${CONF_NAME%.conf}-<suffix>.conf；
       - systemd 单元：新建 ${UNIT_NAME%.service}-v2.service 并改用独立端口。
       若确认要覆盖，请人工操作——本脚本不做覆盖。"
  fi
done
ok "三个目标路径均不存在，可安全新增"

CERT="/etc/nginx/cert/qiubite.fun.crt"
KEY="/etc/nginx/cert/qiubite.fun.key"
[ -r "$CERT" ] || die "证书不可读：$CERT"
[ -r "$KEY"  ] || die "私钥不可读：$KEY"
ok "证书与私钥可读"

# 证书必须覆盖本次使用的 server_name，否则握手会失败
if command -v openssl >/dev/null 2>&1; then
  SAN="$(openssl x509 -in "$CERT" -noout -text 2>/dev/null | awk '/X509v3 Subject Alternative Name/{getline; print}' || true)"
  if [ -n "$SAN" ]; then
    if printf '%s' "$SAN" | grep -q "DNS:[*]\.${SERVER_NAME#*.}"; then
      ok "证书 SAN 覆盖通配 ${SERVER_NAME#*.}"
    elif printf '%s' "$SAN" | grep -q "DNS:${SERVER_NAME}\b"; then
      ok "证书 SAN 精确覆盖 $SERVER_NAME"
    else
      die "证书 SAN 未覆盖 $SERVER_NAME。当前 SAN：$SAN
       处理方式：改用证书已覆盖的域名（--server-name），或为该域名重新签发证书后再部署。
       注意：不能复用 apex 域名——那需要修改既有站点配置，违反「只新增」约束。"
    fi
  else
    warn "未解析到 SAN，跳过域名覆盖检查（握手若失败请先确认证书）"
  fi
fi

# 同一 server_name 已存在 → 新增的站点会被 nginx 忽略，属于无效操作
if command -v nginx >/dev/null 2>&1; then
  if nginx -T 2>/dev/null | grep -Eq "^[^#]*server_name[^;]*\b${SERVER_NAME}\b"; then
    die "已存在站点使用了 server_name ${SERVER_NAME}；nginx 只会保留先加载的那个。
       处理方式：换一个未被占用的子域名（--server-name），或复用既有站点（需改现有文件，本脚本不支持）。"
  fi
  ok "server_name 未被占用"
else
  die "未找到 nginx 命令"
fi

if ss -lntH 2>/dev/null | awk '{print $4}' | grep -q ":${LISTEN_PORT}$"; then
  die "端口 ${LISTEN_PORT} 已被占用；请先确认是否已有同名服务在跑"
fi
ok "端口 ${LISTEN_PORT} 空闲"

if [ "$DRY_RUN" -eq 1 ]; then
  log "干跑结束，未做任何改动"
  printf '将新增：\n  %s\n  %s\n  %s\n' "$BIN_PATH" "$CONF_PATH" "$UNIT_PATH"
  [ "$USE_AUTH" -eq 1 ] && printf '  %s\n' "$AUTH_FILE"
  exit 0
fi

# ------------------------------------------------------------------
# 2. 落盘：只新增
# ------------------------------------------------------------------
log "创建应用目录"
mkdir -p "$APP_DIR/data"
chown -R "${SVC_USER}:${SVC_USER}" "$APP_DIR"
chmod 750 "$APP_DIR"
ok "$APP_DIR"

log "安装二进制"
install -m 0755 -o "$SVC_USER" -g "$SVC_USER" "$BINARY" "$BIN_PATH"
ok "$BIN_PATH （$(du -h "$BIN_PATH" | cut -f1)）"

if [ "$DO_SYSTEMD" -eq 1 ]; then
  log "安装 systemd 单元"
  SRC_UNIT="$(dirname "$0")/../systemd/job-workspace.service"
  [ -f "$SRC_UNIT" ] || die "缺少单元模板：$SRC_UNIT"
  sed -e "s#^User=.*#User=${SVC_USER}#" \
      -e "s#^Group=.*#Group=${SVC_USER}#" \
      -e "s#--addr=127.0.0.1:8787#--addr=127.0.0.1:${LISTEN_PORT}#" \
      "$SRC_UNIT" > "$UNIT_PATH"
  chmod 644 "$UNIT_PATH"
  ok "$UNIT_PATH"

  systemctl daemon-reload
  systemctl enable --now "$UNIT_NAME"
  sleep 2
  systemctl is-active --quiet "$UNIT_NAME" || die "服务未启动，请查看 journalctl -u ${UNIT_NAME} -n 50"
  ok "服务已启动并设为开机自启"
fi

if [ "$DO_NGINX" -eq 1 ]; then
  log "安装 nginx 站点配置"
  SRC_CONF="$(dirname "$0")/../nginx/job-workspace.conf"
  [ -f "$SRC_CONF" ] || die "缺少配置模板：$SRC_CONF"
  sed -e "s#server_name job\.qiubite\.fun;#server_name ${SERVER_NAME};#g" \
      "$SRC_CONF" > "$CONF_PATH"
  chmod 644 "$CONF_PATH"

  if [ "$USE_AUTH" -eq 1 ]; then
    if [ -e "$AUTH_FILE" ]; then
      warn "已存在 $AUTH_FILE，保留原文件不覆盖，沿用旧凭据"
    else
      PASS="$(openssl rand -base64 18 | tr -dc 'A-Za-z0-9' | cut -c1-16)"
      printf '%s:%s\n' "$SVC_USER" "$(openssl passwd -apr1 "$PASS")" > "$AUTH_FILE"
      chmod 640 "$AUTH_FILE"
      GRP="www-data"; getent group "$GRP" >/dev/null 2>&1 || GRP="root"
      chown "root:${GRP}" "$AUTH_FILE"
      ok "$AUTH_FILE"
      printf '\n\033[1;33m  Basic Auth 凭据（仅显示一次，请立即保存）\033[0m\n  用户名: %s\n  密码:   %s\n\n' "$SVC_USER" "$PASS"
      PASS=""
    fi
  else
    # 未启用鉴权时，把 auth_basic 两行注释掉，避免 configtest 因缺文件失败
    sed -i.bak -e 's#^\(\s*\)auth_basic#\1\# auth_basic#' \
               -e 's#^\(\s*\)auth_basic_user_file#\1\# auth_basic_user_file#' "$CONF_PATH"
    rm -f "${CONF_PATH}.bak"
    warn "已按要求关闭访问控制——该站点将裸露在公网"
  fi
  ok "$CONF_PATH"

  log "校验 nginx 配置"
  if ! nginx -t; then
    rm -f "$CONF_PATH"
    die "nginx -t 失败，已撤回本次新增的站点配置（未改动任何既有文件）"
  fi
  ok "nginx -t 通过"
  nginx -s reload
  ok "nginx 已重载"
fi

# ------------------------------------------------------------------
# 3. 验收
# ------------------------------------------------------------------
log "验收"
curl -fsS -m 5 "http://127.0.0.1:${LISTEN_PORT}/api/health" && echo
ok "本机 8787 健康检查通过"

if [ "$DO_NGINX" -eq 1 ]; then
  curl -kfsS -m 8 -o /dev/null -w "  https 状态码: %{http_code}\n" "https://${SERVER_NAME}/" \
    || warn "公网访问未通过（可能是 DNS 未解析到本机，或 Basic Auth 返回 401——401 属正常）"
fi

log "完成。未删除、未覆盖任何既有文件。"
