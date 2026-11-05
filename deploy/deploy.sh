#!/usr/bin/env bash
# Build on the laptop and copy the binary to the VPS (C2). Usage: deploy/deploy.sh [host]
set -euo pipefail
HOST=${1:-kritz}
DIR=wagoya
ARCH=${ARCH:-amd64} # C4: kritz is x86_64
cd "$(dirname "$0")/.."
go test ./...
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags='-s -w' -o dist/wagoya .

# One-time migration from the old name: stop the old service and move its
# folder (with .env and data/, so the WhatsApp link survives).
ssh "$HOST" "set -e
  if [ -d ~/wa-claude-bot ] && [ ! -e ~/$DIR ]; then
    sudo systemctl disable --now wa-claude-bot 2>/dev/null || true
    sudo rm -f /etc/systemd/system/wa-claude-bot.service
    mv ~/wa-claude-bot ~/$DIR
    rm -f ~/$DIR/wa-claude-bot ~/$DIR/wa-claude-bot.service
    echo 'migrated ~/wa-claude-bot -> ~/$DIR'
  fi
  mkdir -p ~/$DIR && chmod 700 ~/$DIR"

scp dist/wagoya "$HOST:$DIR/wagoya.new"
scp deploy/wagoya.service .env.example "$HOST:$DIR/"
ssh "$HOST" "set -e; cd ~/$DIR
  mv wagoya.new wagoya
  [ -f .env ] || { cp .env.example .env; echo 'created .env from template: fill it in'; }
  chmod 600 .env; mkdir -p data; chmod 700 data
  sudo install -m 644 wagoya.service /etc/systemd/system/wagoya.service
  sudo systemctl daemon-reload
  sudo systemctl enable wagoya >/dev/null
  if [ -f data/session.db ]; then sudo systemctl restart wagoya; echo restarted; else echo 'not linked yet: run ./wagoya once interactively to link'; fi"
