#!/bin/sh
set -e
cd "$(dirname "$0")"
install -o root -g root -m 0644 10-remote.conf /etc/rsyslog.d/10-remote.conf
rsyslogd -N1
chgrp -R adm /var/log/remote
systemctl restart rsyslog
ls -la /var/log/remote/
