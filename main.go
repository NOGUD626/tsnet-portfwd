// tsnet-portfwd: tsnet を使った「ssh -R 相当」の port forward。
//
// 「tailnet 内 :LISTEN_PORT に来た TCP を、 実行ホストの TARGET (host:port) に転送」 する。
//
// ssh -R 等価例:
//   従来:  ssh -R 15555:localhost:5555 user@vpnsv -N
//   tsnet: vpnsv で  ./tsnet-portfwd -listen :15555 -target localhost:5555
//
// 違い:
//   - tailnet ノードとして常駐 (= ssh セッション切れても継続)
//   - 認証は Headscale (tailnet 端末認証)
//   - tailnet 全体に MagicDNS で公開
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	var (
		hostname   = flag.String("hostname", "portfwd", "tailnet 上のホスト名")
		listenAddr = flag.String("listen", ":15555", "tailnet 内で listen するアドレス (例: :15555)")
		target     = flag.String("target", "localhost:5555", "転送先 host:port")
		controlURL = flag.String("control-url", "https://headscale.example.com", "Headscale の URL")
		stateDir   = flag.String("state-dir", "./portfwd-state", "tsnet state dir")
		verbose    = flag.Bool("v", false, "tsnet の詳細ログを表示")
	)
	flag.Parse()

	logf := func(format string, args ...any) {}
	if *verbose {
		logf = log.Printf
	}

	// -------- 1. tsnet ノード起動 --------
	srv := &tsnet.Server{
		Hostname:   *hostname,
		Dir:        *stateDir,
		ControlURL: *controlURL,
		AuthKey:    os.Getenv("TS_AUTHKEY"),
		Logf:       logf,
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("--- Starting tsnet portfwd ---\n")
	fmt.Printf("Hostname:   %s\n", *hostname)
	fmt.Printf("ControlURL: %s\n", *controlURL)
	fmt.Printf("Listen:     tailnet %s\n", *listenAddr)
	fmt.Printf("Target:     %s (実行ホストのローカル)\n", *target)
	fmt.Println()

	st, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("tsnet up failed: %v", err)
	}
	fmt.Printf("OK joined tailnet as %s %v\n\n", *hostname, st.TailscaleIPs)

	// -------- 2. tailnet 内で listen --------
	ln, err := srv.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	fmt.Printf("Listening on tailnet %s%s\n", *hostname, *listenAddr)
	fmt.Printf("Forward to: %s\n", *target)
	fmt.Printf("Access from Mac (一例):\n")
	fmt.Printf("  nc %s %s\n", st.TailscaleIPs[0].String(), (*listenAddr)[1:])
	fmt.Printf("  nc %s%s   (MagicDNS)\n\n", *hostname, *listenAddr)

	// -------- 3. SIGINT/SIGTERM で graceful shutdown --------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("signal received, shutting down")
		ln.Close()
	}()

	// -------- 4. accept loop --------
	var connCount atomic.Int64
	for {
		remote, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || err.Error() == "tsnet: listener closed" {
				break
			}
			log.Printf("Accept: %v", err)
			continue
		}
		id := connCount.Add(1)
		go handleConn(id, remote, *target)
	}
	fmt.Println("=== exited ===")
}

// handleConn は 1 接続を target にプロキシする。
func handleConn(id int64, remote net.Conn, target string) {
	defer remote.Close()
	srcInfo := remote.RemoteAddr().String()
	log.Printf("[%04d] %s -> %s", id, srcInfo, target)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	local, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", target)
	if err != nil {
		log.Printf("[%04d] dial %s failed: %v", id, target, err)
		return
	}
	defer local.Close()

	// 双方向 copy (これが ssh -R の中身に相当)
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(local, remote)
		log.Printf("[%04d] remote -> local closed (%d bytes)", id, n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(remote, local)
		log.Printf("[%04d] local -> remote closed (%d bytes)", id, n)
		done <- struct{}{}
	}()
	<-done
}
