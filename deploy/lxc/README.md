# noc-bot を Proxmox の LXC で動かす

## 1. コンテナを作る（Proxmox）

| 項目 | 値 |
| --- | --- |
| ノード | **pve-03**（VM への割り当て合計が実メモリ以内なのはここだけ） |
| テンプレート | Debian 12 か Ubuntu 24.04、**非特権（unprivileged）** |
| CPU / メモリ / ディスク | 1 コア / 1 GiB（swap 512 MiB） / 8 GB |
| ネットワーク | VLAN 110、固定 IP（例 10.110.51.5/16）、GW・DNS は syslog サーバーと同じ |
| 自動起動 | on |

## 2. Tailscale を LXC で使えるようにする（Proxmox ホストで）

非特権 LXC には `/dev/net/tun` がないので、コンテナの設定に2行足して再起動する。

```sh
# pve-03 で。<CTID> はコンテナ番号
cat >> /etc/pve/lxc/<CTID>.conf <<'CONF'
lxc.cgroup2.devices.allow: c 10:200 rwm
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file
CONF
pct reboot <CTID>
```

## 3. コンテナの中で

```sh
curl -fsSL https://tailscale.com/install.sh | sh
tailscale up --hostname noc-bot --advertise-tags=tag:noc-bot   # tag は tailnet の ACL で owner を設定してから
sh deploy/lxc/install.sh ./noc-bot     # バイナリは CGO_ENABLED=0 go build ./cmd/noc-bot
vi /etc/noc-bot/noc-bot.yaml /etc/noc-bot/env
systemctl enable --now noc-bot
curl -s localhost:8080/health
```

`--accept-routes` は付けない（コンテナはもう 10.110 にいるので、subnet route を取ると経路が二重になる）。

## 4. tailnet の ACL（例）

```jsonc
"tagOwners": { "tag:noc-bot": ["autogroup:admin"], "tag:laya": ["autogroup:admin"] },
"acls": [
  { "action": "accept", "src": ["tag:laya"],   "dst": ["tag:noc-bot:8080"] },  // Escalator -> /v1/teacher
  { "action": "accept", "src": ["tag:noc-bot"], "dst": ["tag:laya:8100"] }     // noc-bot -> laya-svc
]
```

laya-svc を動かす Mac も同じ tailnet に `tag:laya` で入れる。設定する URL：

* Mac 側 `LAYA_SVC_TEACHER_URL=http://noc-bot:8080/v1/teacher`（MagicDNS）
* noc-bot 側 `laya.url: http://<Mac の tailnet 名>:8100`

## 5. syslog サーバーから転送を始める

`deploy/rsyslog-forward.conf` の `NOC_BOT_HOST` をコンテナの IP にして、
`/etc/rsyslog.d/05-noc-bot.conf` に置き、`rsyslogd -N1` で確認してから `systemctl restart rsyslog`。
転送キューはあふれたら捨てるので、noc-bot が止まってもログの保存には影響しない。
