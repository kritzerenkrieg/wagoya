#!/usr/bin/env bash
# Build on the laptop and copy the binary to the VPS (C2). Usage: deploy/deploy.sh [host]
set -euo pipefail
HOST=${1:-kritz}
DIR=wagoya
ARCH=${ARCH:-amd64} # C4: kritz is x86_64
cd "$(dirname "$0")/.."
go test ./...
# J6: golden set against the real model (about $0.002 a run). Skip with SKIP_EVAL=1.
if [ -z "${SKIP_EVAL:-}" ]; then
  out=$(EVAL=1 go test ./internal/eval -count=1 -v 2>&1) && ok=1 || ok=
  grep -E -- '--- FAIL|golden set|try [0-9]' <<<"$out" || true
  [ -n "$ok" ] || { echo 'golden set failed: not deploying (SKIP_EVAL=1 to override)'; exit 1; }
fi
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

# The 27 MB upload often stalls on a slow link: rsync gives up after 30 s of
# silence and the next try resumes the partial file. The checksum must match
# before anything is installed.
for try in 1 2 3 4 5; do
  rsync --partial --append-verify --timeout=30 dist/wagoya "$HOST:$DIR/wagoya.new" && break
  [ "$try" = 5 ] && { echo 'upload failed 5 times: not deploying (old binary still running)'; exit 1; }
  echo "upload stalled, resuming ($try)"
done
want=$(sha256sum dist/wagoya | cut -d' ' -f1)
got=$(ssh "$HOST" "sha256sum ~/$DIR/wagoya.new | cut -d' ' -f1")
[ "$want" = "$got" ] || { echo 'uploaded binary does not match: not deploying'; ssh "$HOST" "rm -f ~/$DIR/wagoya.new"; exit 1; }
rsync --timeout=30 deploy/wagoya.service .env.example "$HOST:$DIR/"
ssh "$HOST" "set -e; cd ~/$DIR
  chmod 755 wagoya.new # a resumed rsync partial can lose the exec bit
  mv wagoya.new wagoya
  [ -f .env ] || { cp .env.example .env; echo 'created .env from template: fill it in'; }
  chmod 600 .env; mkdir -p data; chmod 700 data
  sudo install -m 644 wagoya.service /etc/systemd/system/wagoya.service
  sudo systemctl daemon-reload
  sudo systemctl enable wagoya >/dev/null
  if [ ! -f data/session.db ]; then echo 'not linked yet: run ./wagoya once interactively to link'; exit 0; fi
  sudo systemctl restart wagoya
  for i in \$(seq 1 20); do
    sleep 1
    inv=\$(systemctl show -p InvocationID --value wagoya)
    if journalctl _SYSTEMD_INVOCATION_ID=\$inv --no-pager | grep -q 'connected as'; then echo restarted; exit 0; fi
  done
  echo 'restarted, but not connected after 20 s:'; systemctl status wagoya --no-pager | head -5; exit 1"
