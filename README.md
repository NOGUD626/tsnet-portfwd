# tsnet-portfwd

> A `ssh -R`-equivalent port forwarder built on **tsnet**. Expose any local or LAN-reachable service to your self-hosted **Headscale** tailnet — without ssh sessions or open firewall ports.

[![Go Reference](https://pkg.go.dev/badge/tailscale.com/tsnet.svg)](https://pkg.go.dev/tailscale.com/tsnet)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Go 1.24+](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)

## 概要

`ssh -R REMOTE_PORT:HOST:PORT user@gateway -N` の挙動を、 [`tailscale.com/tsnet`](https://pkg.go.dev/tailscale.com/tsnet) で書き直した単機能 daemon。

| | `ssh -R` | tsnet-portfwd |
|---|---|---|
| 認証 | SSH パスワード / 鍵 | **Tailscale 端末認証** (+ Headscale ACL) |
| セッション | ssh セッションに紐づく、 切れたら停止 | **プロセス常駐**、 ssh セッション不要 |
| 公開範囲 | サーバ側の `localhost` (or `GatewayPorts yes`) | **tailnet 全体に MagicDNS で公開** |
| 暗号化 | SSH トンネル | **WireGuard (P2P) or DERP リレー** |
| 多重化 | 1 セッションで複数 forward | プロセス 1 つで listen 何本でも |
| 障害分離 | SSH 全体が落ちる | port forward 単体プロセスだけ kill 可能 |

## アーキテクチャ

```
[Client (e.g. your laptop)]                                     
                                                                 
 ┌──────────────────┐                                            
 │ Tailscale client │                                            
 │ (official app or │                                            
 │  another tsnet)  │                                            
 └────────┬─────────┘                                            
          │  nc <portfwd-hostname> <LISTEN_PORT>                 
          │  curl http://<portfwd-hostname>:<LISTEN_PORT>/       
          ▼                                                       
    [tailnet] (WireGuard P2P or DERP)                            
          │                                                       
          ▼                                                       
[Gateway machine (e.g. your home server)]                        
                                                                 
 ┌──────────────────────────────────────┐                        
 │ tsnet-portfwd process                │                        
 │   srv.Listen(":LISTEN_PORT")         │                        
 │   net.Dial("TARGET_HOST:PORT")       │                        
 │   double io.Copy()                   │                        
 └────────┬─────────────────────────────┘                        
          │                                                       
          ▼                                                       
   [TARGET (localhost or any LAN host)]                          
   e.g. localhost:5432 (Postgres)                                
        192.168.X.X:80 (internal nginx)                          
```

## Prerequisites

- Go 1.21+ on the build machine
- Self-hosted [Headscale](https://github.com/juanfont/headscale) (v0.28+ recommended)
- A target machine where you'll run `tsnet-portfwd` (it needs network reach to the `-target` host:port)

## Quick Start

### 1. Build (with optional cross-compile for Linux servers)

```bash
git clone https://github.com/NOGUD626/tsnet-portfwd.git
cd tsnet-portfwd
go mod tidy
go build -o tsnet-portfwd .                          # native (host OS)

# Or cross-compile for a Linux server
GOOS=linux GOARCH=amd64 go build -o tsnet-portfwd-linux .
scp tsnet-portfwd-linux user@gateway:/tmp/
```

### 2. Issue a pre-auth key

```bash
ssh <HEADSCALE_HOST> 'sudo headscale preauthkey create -u <USER_ID> -e 1h --reusable'
# → "hskey-auth-XXXX..." を取得
```

### 3. Run on the gateway machine

```bash
export TS_AUTHKEY=hskey-auth-XXXX...
./tsnet-portfwd \
    -control-url https://headscale.example.com \
    -hostname my-portfwd \
    -listen :15555 \
    -target localhost:5432
```

### 4. Access from any tailnet peer

```bash
# MagicDNS-aware client
nc my-portfwd 15555

# Or via tailnet IP directly
nc 100.64.0.X 15555
```

## Usage

```text
Usage of ./tsnet-portfwd:
  -hostname string
        tailnet 上のホスト名 (default "portfwd")
  -listen string
        tailnet 内で listen するアドレス (default ":15555")
  -target string
        転送先 host:port (default "localhost:5555")
  -control-url string
        Headscale の URL (default "https://headscale.example.com")
  -state-dir string
        tsnet state dir (default "./portfwd-state")
  -v    tsnet の詳細ログを表示
```

## Examples

### 1) Expose a local PostgreSQL to tailnet (classic `ssh -R 5432:localhost:5432`)

```bash
./tsnet-portfwd -hostname pg-bridge -listen :5432 -target localhost:5432
# Any peer:
psql -h pg-bridge -p 5432 -U user dbname
```

### 2) Expose another LAN host through this gateway

`-target` is not limited to `localhost` — anything reachable from the gateway's OS routing works:

```bash
./tsnet-portfwd -hostname web-bridge -listen :18080 -target 192.168.X.X:80
# Any peer:
curl http://web-bridge:18080/
curl -H 'Host: internal-app.example' http://web-bridge:18080/
```

### 3) Bridge SSH itself (so peers can ssh through your gateway by hostname)

```bash
./tsnet-portfwd -hostname ssh-bridge -listen :12222 -target localhost:22
ssh -p 12222 user@ssh-bridge
```

### 4) Multiple forwards on the same machine

Just run multiple processes with different `-hostname` / `-listen` / `-state-dir`:

```bash
./tsnet-portfwd -hostname pg-bridge  -listen :5432 -target localhost:5432  -state-dir ./pg-state &
./tsnet-portfwd -hostname web-bridge -listen :18080 -target 192.168.X.X:80 -state-dir ./web-state &
```

Each registers as its **own tailnet node** with its own Tailscale IP.

## How it works

```
1. srv.Up()                  : join the tailnet, get a Tailscale IP
2. ln := srv.Listen(LISTEN)  : start a userspace TCP listener inside tsnet
3. for { conn := ln.Accept() ; go handle(conn) }
4. handle:
     local, _ := net.Dial("tcp", TARGET)
     go io.Copy(local, conn)       // peer  → target
     go io.Copy(conn, local)       // target → peer
```

中身は **double `io.Copy`** だけ。 ssh -R が `ssh` の bytestream の中でやっているのと同じことを、 WireGuard の bytestream の中でやっている。

## Troubleshooting

| 症状 | 対処 |
|---|---|
| `tsnet up failed: 401 Unauthorized` | AuthKey が期限切れ or 既に使用済。 新規発行 (`headscale preauthkey create`) |
| `tsnet up failed: tls handshake failed` | Headscale 側の TLS / リバースプロキシ設定を確認 |
| `Dial: i/o timeout` (target が LAN ホスト) | gateway の OS から target に到達できるか確認、 同 LAN なら問題ないはず |
| MagicDNS で名前解決できない | クライアント側で `tailscale set --accept-dns=true` |
| 2 回目に hostname 重複エラー | `headscale nodes delete -i <ID>` で旧ノード削除 or `-state-dir` を変える |

## Cleanup

```bash
# 1. プロセス停止
pkill tsnet-portfwd

# 2. ローカル state
rm -rf ./portfwd-state

# 3. Headscale 側からノード削除
ssh <HEADSCALE_HOST> 'sudo headscale nodes list'
ssh <HEADSCALE_HOST> 'sudo headscale nodes delete -i <ID> --force'
```

## Extension ideas

| アイデア | 実装の方向 |
|---|---|
| **WhoIs ベース ACL** | `LocalClient().WhoIs(conn.RemoteAddr())` で接続元 Tailscale ID 取得 → 許可リスト判定 |
| **TLS 終端** | `srv.ListenTLS(":443")` で Let's Encrypt 連携相当の HTTPS リバプロ化 |
| **複数 target round-robin** | target を `host1:80,host2:80` カンマ区切り、 順番に Dial |
| **アクセスログ** | accept のたびに JSON で structured log 出力 |
| **メトリクス** | Prometheus exporter を同居させる |

## Related

- [tsnet package documentation](https://pkg.go.dev/tailscale.com/tsnet)
- [Headscale](https://github.com/juanfont/headscale) — Open source, self-hosted Tailscale coordination server
- [`tsnet-demo`](https://github.com/NOGUD626/tsnet-demo) — Sister project: tailnet join + Ping/Dial verification
- [shayne/tsnet-serve](https://github.com/shayne/tsnet-serve) — More feature-rich, with TLS / Funnel / path filtering

## License

[MIT](LICENSE)

Tailscale and `tsnet` themselves are © Tailscale Inc., licensed under [BSD-3-Clause](https://github.com/tailscale/tailscale/blob/main/LICENSE).
