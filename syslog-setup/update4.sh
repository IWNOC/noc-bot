#!/bin/sh
# syslog サーバーで sudo sh update4.sh
set -e
cd "$(dirname "$0")"
cp /etc/rsyslog.d/10-remote.conf /etc/rsyslog.d/10-remote.conf.bak
install -o root -g root -m 0644 10-remote.conf /etc/rsyslog.d/10-remote.conf
if ! rsyslogd -N1; then mv /etc/rsyslog.d/10-remote.conf.bak /etc/rsyslog.d/10-remote.conf; echo "config error: rolled back"; exit 1; fi
rm -f /etc/rsyslog.d/10-remote.conf.bak
systemctl restart rsyslog
df -h /var/log/remote
echo done
