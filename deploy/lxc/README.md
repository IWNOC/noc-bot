# noc-bot を Proxmox の LXC で動かす

## 1. コンテナを作る（Proxmox）

| 項目 | 値 |
| --- | --- |
| ノード | **pve-03**（VM への割り当て合計が実メモリ以内なのはここだけ） |
| テンプレート | Debian 12 か Ubuntu 24.04、**非特権（unprivileged）** |
| CPU / メモリ / ディスク | 1 コア / 1 GiB（swap 512 MiB） / 8 GB |
| ネットワーク | VLAN 110、固定 IP（例 10.110.51.5/16）、GW・DNS は syslog サーバーと同じ |
| 自動起動 | on |

## 2. Tailscale（ユーザー空間モード）

非特権 LXC には `/dev/net/tun` がない。渡すには Proxmox ホストで root@pam が設定を足す必要があるので、
**TUN を使わないユーザー空間モード**で動かす（ホスト側の変更なし）。

```sh
# コンテナの中で（公式 apt リポジトリ）
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list -o /etc/apt/sources.list.d/tailscale.list
apt-get update && apt-get install -y tailscale
sed -i 's|^FLAGS=.*|FLAGS="--tun=userspace-networking --outbound-http-proxy-listen=127.0.0.1:1055"|' /etc/default/tailscaled
systemctl restart tailscaled
tailscale up --hostname noc-bot --accept-routes=false   # 表示された URL でログイン
```

* **受信**（laya-svc → noc-bot の /v1/teacher）：tailscaled が 127.0.0.1:8080 に中継する。nftables は lo を許可しているので通る。
  届く範囲は tailnet の ACL で絞る。
* **送信**（noc-bot → laya-svc）：tailscaled の HTTP プロキシを通す。`/etc/noc-bot/env` に
  `HTTP_PROXY=http://127.0.0.1:1055` と `NO_PROXY=localhost,127.0.0.1` を書く。
  HTTPS（ローカル LLM、Slack）は `HTTPS_PROXY` を設定しないのでプロキシを通らない。
* サーバーなので、管理画面でこのノードの鍵の有効期限（key expiry）を無効にしておく。

## 3. noc-bot を入れる

```sh
sh deploy/lxc/install.sh ./noc-bot     # バイナリは CGO_ENABLED=0 go build ./cmd/noc-bot
vi /etc/noc-bot/noc-bot.yaml /etc/noc-bot/env
systemctl enable --now noc-bot
curl -s localhost:8080/health
```

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
