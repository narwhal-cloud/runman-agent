// streamtest 是一个 gRPC 长连接稳定性诊断工具，用于定位 agent 断流问题。
//
// 它复刻真实 agent 的连接方式（Connect 双向流 + Bearer 认证 + 相同的 keepalive 参数），
// 但不跑任何业务逻辑：上行只发 ConsoleOutput（session 不存在，平台侧直接忽略，零副作用），
// 下行只统计 Ping。记录每条连接的存活时长，用于对比不同链路：
//
//	直连（SSH 隧道，绕过 CF+Caddy）:
//	  ssh -N -L 2990:127.0.0.1:2990 user@server
//	  go run ./cmd/streamtest -token <TOKEN>
//
//	完整链路（经 Cloudflare + Caddy）:
//	  go run ./cmd/streamtest -token <TOKEN> -addr hosting.fuckip.me:443 -tls
//
//	绕过 CF、直连源站 Caddy（-host 指定 SNI/:authority 命中 site block；
//	源站是 CF Origin CA 自签证书，需 -skip-verify）:
//	  go run ./cmd/streamtest -token <TOKEN> -addr <源站IP>:443 -tls -host hosting.fuckip.me -skip-verify
//
// 注意：-token 必须使用没有真实 agent 在线的测试机 token。平台按 token 对应的
// machineID 路由命令，用在线机器的 token 会把该机器的命令/控制台流量劫持到本工具。
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"time"

	pb "runman-agent/proto/agent"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2990", "gRPC 服务地址")
	token := flag.String("token", "", "agent token（必须是无真实 agent 在线的测试机）")
	useTLS := flag.Bool("tls", false, "启用 TLS（走 Caddy/CF 完整链路）；默认 h2c 明文（SSH 隧道直连）")
	host := flag.String("host", "", "覆盖 TLS SNI 和 :authority（配合 -addr <IP>:443 绕过 CF 直连源站 Caddy）")
	skipVerify := flag.Bool("skip-verify", false, "跳过 TLS 证书验证（源站为 CF Origin CA 自签证书时使用）")
	interval := flag.Duration("interval", 15*time.Second, "上行发送间隔，0 表示不发上行（只收 Ping）")
	size := flag.Int("size", 4096, "每条上行消息的 payload 字节数")
	duration := flag.Duration("duration", 12*time.Minute, "测试总时长")
	kaTime := flag.Duration("ka-time", 12*time.Second, "客户端 keepalive Time（与真实 agent 一致），0 禁用")
	flag.Parse()

	if *token == "" {
		log.Fatal("-token 必填")
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	deadline := time.Now().Add(*duration)
	var lifetimes []time.Duration
	for run := 1; time.Now().Before(deadline); run++ {
		lt, err := runOnce(run, *addr, *token, *useTLS, *host, *skipVerify, *interval, *size, *kaTime, deadline)
		lifetimes = append(lifetimes, lt)
		st, _ := status.FromError(err)
		log.Printf("[run %d] 流结束，存活 %s，code=%s err=%v",
			run, lt.Round(time.Millisecond), st.Code(), err)
		if time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
		}
	}

	log.Printf("==== 结果汇总（%d 条连接）====", len(lifetimes))
	for i, lt := range lifetimes {
		log.Printf("  run %-2d 存活 %s", i+1, lt.Round(time.Second))
	}
}

// runOnce 建立一条 Connect 双向流并维持到出错或到达 deadline，返回流的存活时长。
func runOnce(run int, addr, token string, useTLS bool, host string, skipVerify bool, interval time.Duration, size int, kaTime time.Duration, deadline time.Time) (time.Duration, error) {
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{
			ServerName:         host, // 空串时 grpc 自动取 addr 的 host 部分
			InsecureSkipVerify: skipVerify,
		})
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if host != "" {
		opts = append(opts, grpc.WithAuthority(host))
	}
	if kaTime > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                kaTime,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

	stream, err := pb.NewAgentGatewayClient(conn).Connect(ctx)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	log.Printf("[run %d] 流已建立 → %s (tls=%v, 上行间隔=%s, payload=%dB)", run, addr, useTLS, interval, size)

	// 上行流量：ConsoleOutput 指向不存在的 session，平台侧查不到会直接丢弃，
	// 只用于复刻 agent 心跳的"客户端→服务端持续上行"流量形态。
	if interval > 0 {
		payload := make([]byte, size)
		_, _ = rand.Read(payload)
		sessionID := fmt.Sprintf("streamtest-%d", time.Now().UnixNano())
		go func() {
			seq := 0
			send := func() bool {
				seq++
				err := stream.Send(&pb.AgentEnvelope{
					MessageId: fmt.Sprintf("%s-%d", sessionID, seq),
					Payload: &pb.AgentEnvelope_ConsoleOutput{ConsoleOutput: &pb.ConsoleOutput{
						SessionId: sessionID,
						Data:      payload,
					}},
				})
				if err != nil {
					log.Printf("[run %d] 上行发送失败 (+%s): %v", run, time.Since(start).Round(time.Millisecond), err)
					return false
				}
				log.Printf("[run %d] ↑ 已发送 #%d (+%s)", run, seq, time.Since(start).Round(time.Millisecond))
				return true
			}
			if !send() { // 与真实 agent 一致：连上立即发一次
				return
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if !send() {
						return
					}
				case <-stream.Context().Done():
					return
				}
			}
		}()
	}

	// 下行读取循环，阻塞直到流断开。
	for {
		env, err := stream.Recv()
		if err != nil {
			return time.Since(start), err
		}
		switch env.Payload.(type) {
		case *pb.PlatformEnvelope_Ping:
			log.Printf("[run %d] ↓ 收到 Ping (+%s)", run, time.Since(start).Round(time.Millisecond))
		default:
			log.Printf("[run %d] ↓ 收到 %T (+%s) —— 警告：该 token 的机器有真实命令下发！", run, env.Payload, time.Since(start).Round(time.Millisecond))
		}
	}
}
