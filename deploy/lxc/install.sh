#!/bin/sh
# Run inside the noc-bot LXC as root:  sh install.sh ./noc-bot
# Installs the binary, a service user, config skeleton, firewall, and the systemd unit.
set -eu
BIN=${1:?usage: install.sh <path to noc-bot binary>}
HERE=$(cd "$(dirname "$0")" && pwd)

install -m 0755 "$BIN" /usr/local/bin/noc-bot
id noc-bot >/dev/null 2>&1 || useradd --system --home /var/lib/noc-bot --shell /usr/sbin/nologin noc-bot
install -d -o noc-bot -g noc-bot -m 0750 /var/lib/noc-bot
install -d -m 0750 -g noc-bot /etc/noc-bot
[ -f /etc/noc-bot/noc-bot.yaml ] || install -m 0640 -g noc-bot "$HERE/../../noc-bot.example.yaml" /etc/noc-bot/noc-bot.yaml
if [ ! -f /etc/noc-bot/env ]; then
  umask 077
  cat > /etc/noc-bot/env <<'ENV'
# secrets: fill these in, keep mode 0640 root:noc-bot
LLM_TOKEN=
LAYA_SVC_TOKEN=
NOC_BOT_TEACHER_TOKEN=
SLACK_WEBHOOK_URL=
# SLACK_BOT_TOKEN=
ENV
  chgrp noc-bot /etc/noc-bot/env && chmod 0640 /etc/noc-bot/env
fi
sed -i 's#path: /data/noc-bot.db#path: /var/lib/noc-bot/noc-bot.db#' /etc/noc-bot/noc-bot.yaml

# firewall: syslog stream only from the syslog server, teacher/health only over tailscale and the mgmt LAN
if command -v nft >/dev/null; then
  install -m 0644 "$HERE/nftables-noc-bot.conf" /etc/nftables.conf
  systemctl enable nftables && systemctl restart nftables   # apt may already have started it with the default rules
fi

install -m 0644 "$HERE/noc-bot.service" /etc/systemd/system/noc-bot.service
systemctl daemon-reload
echo "Edit /etc/noc-bot/noc-bot.yaml and /etc/noc-bot/env, then: systemctl enable --now noc-bot"
