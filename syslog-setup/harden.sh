#!/bin/sh
# イベント期間中に倒れないための対策。syslog サーバーで: sudo sh harden.sh
set -e
cd "$(dirname "$0")"

echo "== 1. 自動アップデートを止める（rsyslog の勝手な再起動を防ぐ）"
cat > /etc/apt/apt.conf.d/99-event-freeze <<'X'
APT::Periodic::Update-Package-Lists "0";
APT::Periodic::Unattended-Upgrade "0";
X
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service 2>/dev/null || true

echo "== 2. UDP 受信バッファを増やす"
cat > /etc/sysctl.d/90-syslog.conf <<'X'
net.core.rmem_max = 33554432
net.core.netdev_max_backlog = 5000
X
sysctl -p /etc/sysctl.d/90-syslog.conf
cp /etc/rsyslog.conf /etc/rsyslog.conf.bak
sed -i 's|^input(type="imudp" port="514")$|input(type="imudp" port="514" rcvbufSize="16m")|' /etc/rsyslog.conf
if ! rsyslogd -N1; then mv /etc/rsyslog.conf.bak /etc/rsyslog.conf; echo "rsyslog.conf error: rolled back"; exit 1; fi

echo "== 3. /var/log/remote を専用の領域に分ける（ディスクが埋まってもOSは止まらない）"
if ! mountpoint -q /var/log/remote; then
  lvcreate -y -L 50G -n remote ubuntu-vg
  mkfs.ext4 -q -L remote /dev/ubuntu-vg/remote
  mkdir -p /mnt/remote-new
  mount /dev/ubuntu-vg/remote /mnt/remote-new
  rsync -a /var/log/remote/ /mnt/remote-new/
  systemctl stop rsyslog
  rsync -a /var/log/remote/ /mnt/remote-new/        # 停止中の差分
  umount /mnt/remote-new && rmdir /mnt/remote-new
  mv /var/log/remote /var/log/remote.old
  install -d -o syslog -g adm -m 0750 /var/log/remote
  echo "/dev/ubuntu-vg/remote /var/log/remote ext4 defaults,nofail 0 2" >> /etc/fstab
  systemctl daemon-reload
  mount /var/log/remote
  chown syslog:adm /var/log/remote && chmod 0750 /var/log/remote
fi

systemctl restart rsyslog
sleep 3
systemctl is-active rsyslog
df -h /var/log/remote /
ls /var/log/remote
echo "done (古いデータは /var/log/remote.old。確認後に削除してよい)"
