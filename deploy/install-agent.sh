#!/usr/bin/env bash
# 在一台 cell 主机上安装/更新 cell-agent(systemd 方式)。
# 前提:主机已装 docker、克隆好 sub2api 仓库(edge-mode 分支)。主机不用装 Go——
# 用 docker 里的 golang 镜像编译。
set -euo pipefail

REPO_DIR="${REPO_DIR:-/opt/sub2api}"
GOPROXY_VAL="${GOPROXY:-https://goproxy.cn,direct}"
BIN=/usr/local/bin/cell-agent

if [ ! -d "$REPO_DIR/backend/cmd/cell-agent" ]; then
  echo "!! 找不到 $REPO_DIR/backend/cmd/cell-agent —— 设 REPO_DIR 指向 sub2api 仓库根" >&2
  exit 1
fi

echo "==> 编译 cell-agent(via docker golang,主机无需装 Go)"
docker run --rm -v "$REPO_DIR/backend":/src -w /src \
  -e GOPROXY="$GOPROXY_VAL" -e CGO_ENABLED=0 -e GOOS=linux \
  golang:1.26.4-alpine go build -o /src/cell-agent-bin ./cmd/cell-agent
install -m 0755 "$REPO_DIR/backend/cell-agent-bin" "$BIN"
rm -f "$REPO_DIR/backend/cell-agent-bin"

echo "==> 安装 systemd 单元"
install -m 0644 "$REPO_DIR/deploy/cell-agent.service" /etc/systemd/system/cell-agent.service
# 单元模板里写死了 /opt/sub2api/deploy;按本机实际仓库路径改写(支持任意 REPO_DIR)。
sed -i "s|/opt/sub2api/deploy|$REPO_DIR/deploy|g" /etc/systemd/system/cell-agent.service

if [ ! -f "$REPO_DIR/deploy/.env.agent" ]; then
  cp "$REPO_DIR/deploy/.env.agent.example" "$REPO_DIR/deploy/.env.agent"
  sed -i "s|^AGENT_DEPLOY_DIR=.*|AGENT_DEPLOY_DIR=$REPO_DIR/deploy|" "$REPO_DIR/deploy/.env.agent"
  echo "!! 已生成 $REPO_DIR/deploy/.env.agent —— 填好 AGENT_TOKEN 再启动:"
  echo "     AGENT_TOKEN=\$(openssl rand -hex 32)"
fi

systemctl daemon-reload
systemctl enable --now cell-agent
systemctl status cell-agent --no-pager | head -5 || true

echo "==> 完成。验证(把 <token> 换成 .env.agent 里的 AGENT_TOKEN):"
echo "     curl -s -H 'Authorization: Bearer <token>' http://127.0.0.1:9099/agent/v1/health"
