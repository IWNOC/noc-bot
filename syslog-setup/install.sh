#!/bin/sh
# syslog サーバーで sudo sh install.sh を実行する
set -e
cd "$(dirname "$0")"
install -d -o syslog -g adm -m 0750 /var/log/remote
install -o root -g root -m 0644 10-remote.conf /etc/rsyslog.d/10-remote.conf
install -o root -g root -m 0755 remote-log-retention /etc/cron.daily/remote-log-retention
install -o root -g root -m 0644 local-ntp.sources /etc/chrony/sources.d/local-ntp.sources
rsyslogd -N1
systemctl restart rsyslog
chronyc reload sources
systemctl restart chrony
sleep 5
chronyc -n sources
echo "done"
