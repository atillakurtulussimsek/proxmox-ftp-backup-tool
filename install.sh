#!/usr/bin/env bash
# pve-ftp-backup kurulum betiği. Proxmox VE host üzerinde root olarak çalıştırın.
# Kullanım: ./install.sh [binary-yolu]
set -euo pipefail

NAME=pve-ftp-backup
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_SRC="${1:-}"
BIN_DST=/usr/local/bin/$NAME
CONF_DIR=/etc/$NAME
DATA_DIR=/var/lib/$NAME
UNIT_DST=/etc/systemd/system/$NAME.service

if [[ $EUID -ne 0 ]]; then
  echo "Bu betik root olarak çalıştırılmalı." >&2
  exit 1
fi

if [[ -z "$BIN_SRC" ]]; then
  for c in "$SRC_DIR/$NAME" "$SRC_DIR/build/$NAME"; do
    [[ -f "$c" ]] && BIN_SRC="$c" && break
  done
fi
if [[ -z "$BIN_SRC" || ! -f "$BIN_SRC" ]]; then
  echo "Binary bulunamadı. Önce 'make build' çalıştırın veya yolu verin: ./install.sh build/$NAME" >&2
  exit 1
fi

UNIT_SRC=""
for c in "$SRC_DIR/deploy/$NAME.service" "$SRC_DIR/$NAME.service"; do
  [[ -f "$c" ]] && UNIT_SRC="$c" && break
done
if [[ -z "$UNIT_SRC" ]]; then
  echo "systemd unit dosyası bulunamadı ($NAME.service)." >&2
  exit 1
fi

if ! command -v vzdump >/dev/null 2>&1 || ! command -v pvesh >/dev/null 2>&1; then
  echo "Uyarı: vzdump/pvesh bulunamadı. Bu araç Proxmox VE host üzerinde çalışmak içindir." >&2
fi

WAS_ACTIVE=0
systemctl is-active --quiet $NAME 2>/dev/null && WAS_ACTIVE=1

echo "→ Binary kopyalanıyor: $BIN_DST"
install -m 0755 -o root -g root "$BIN_SRC" "$BIN_DST.new"
mv -f "$BIN_DST.new" "$BIN_DST"

echo "→ Klasörler oluşturuluyor"
install -d -m 0700 -o root -g root "$CONF_DIR" "$DATA_DIR"

echo "→ systemd servisi kuruluyor"
install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_DST"
systemctl daemon-reload

if [[ ! -f "$CONF_DIR/config.json" ]] || ! grep -q '"passwordHash": "\$2' "$CONF_DIR/config.json"; then
  echo
  echo "→ Web arayüzü için kullanıcı adı ve şifre belirleyin"
  until "$BIN_DST" set-password; do
    echo "Tekrar deneyin."
  done
fi
chmod 600 "$CONF_DIR/config.json"
chown root:root "$CONF_DIR/config.json"

systemctl enable $NAME >/dev/null 2>&1
if [[ $WAS_ACTIVE -eq 1 ]]; then
  echo "→ Servis yeniden başlatılıyor"
  systemctl restart $NAME
else
  echo "→ Servis başlatılıyor"
  systemctl start $NAME
fi

LISTEN=$(grep -o '"listen": *"[^"]*"' "$CONF_DIR/config.json" | sed 's/.*"\([^"]*\)"$/\1/')
PORT=${LISTEN##*:}
SCHEME=http
grep -q '"tlsCert": *"[^"]' "$CONF_DIR/config.json" && SCHEME=https
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo
echo "Kurulum tamamlandı. Arayüz: $SCHEME://${IP:-<sunucu-ip>}:$PORT"
echo "Durum: systemctl status $NAME   ·   Log: journalctl -u $NAME -f"
