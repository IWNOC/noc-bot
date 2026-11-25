#!/bin/sh
# syslog サーバーで sudo sh update3.sh
set -e
cd "$(dirname "$0")"
cp /etc/rsyslog.d/10-remote.conf /etc/rsyslog.d/10-remote.conf.bak
install -o root -g root -m 0644 10-remote.conf /etc/rsyslog.d/10-remote.conf
if ! rsyslogd -N1; then mv /etc/rsyslog.d/10-remote.conf.bak /etc/rsyslog.d/10-remote.conf; echo "config error: rolled back"; exit 1; fi
rm -f /etc/rsyslog.d/10-remote.conf.bak
rm -f /etc/cron.daily/remote-log-retention
install -o root -g root -m 0755 remote-log-retention /etc/cron.hourly/remote-log-retention
systemctl restart rsyslog
# 空いている VG 領域をログ用に追加（オンラインで拡張）
FREE=$(vgs --noheadings --units g -o vg_free ubuntu-vg | tr -dc 0-9.)
echo "VG free: ${FREE}G"
lvextend -r -l +100%FREE /dev/ubuntu-vg/remote || true
df -h /var/log/remote
echo done
