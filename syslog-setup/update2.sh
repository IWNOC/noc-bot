#!/bin/sh
# syslog サーバーで sudo sh update2.sh を実行する
set -e
cd "$(dirname "$0")"
cp /etc/rsyslog.d/10-remote.conf /etc/rsyslog.d/10-remote.conf.bak
install -o root -g root -m 0644 10-remote.conf /etc/rsyslog.d/10-remote.conf
if ! rsyslogd -N1; then
  mv /etc/rsyslog.d/10-remote.conf.bak /etc/rsyslog.d/10-remote.conf; echo "config error: rolled back"; exit 1
fi
rm -f /etc/rsyslog.d/10-remote.conf.bak
install -o root -g root -m 0755 syslog-watch /etc/cron.hourly/syslog-watch
systemctl restart rsyslog
/etc/cron.hourly/syslog-watch
echo done
