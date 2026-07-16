//go:build containers_image_openpgp

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cloudhv"
	"runman-agent/manager/incus"
	"runman-agent/manager/podman"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/ndp"
	"runman-agent/proto/agent"
	"runman-agent/traffic"
	"runman-agent/updater"
	"runman-agent/web"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

var version = "dev"

const serverAddr = "hosting.fuckip.me:443"

// agentConsoleSession 保存一个活跃的控制台 TTY 会话的状态，
// 用于将平台下发的 stdin / resize 消息路由到正确的 AttachTTY 调用。
// stdin 经由 stdinCh 交给会话内的单一写入协程串行写 pipe，
// 保证输入字节顺序，且 pipe 写阻塞时不会波及 gRPC Recv 循环。
type agentConsoleSession struct {
	stdinCh  chan []byte
	resizeCh chan manager.ResizeEvent
	cancel   context.CancelFunc
}

// Agent 是运行在宿主机上的代理进程，负责管理容器生命周期、
// 收集监控指标并通过 gRPC 双向流与平台保持长连接。
type Agent struct {
	cfg           *config.Manager
	db            *db.DB
	mgr           manager.VMManager
	hostMon       *monitor.HostMonitor
	pf            *portforward.Manager
	config        config.Config // 缓存的配置副本（仅在 run() 循环中更新）
	mu            sync.RWMutex
	connected     bool
	lastError     string
	lastConnected time.Time
	entryIPv4     string // 公网 IPv4，启动时自动检测
	entryIPv6     string // 公网 IPv6，启动时自动检测

	// ping 健康检测（用于检测僵尸连接）
	lastPingTime time.Time // 最后收到 Ping 的时间

	// consoleSessions 存储 sessionID (string) → *agentConsoleSession，
	// 跨多个 handleCommand goroutine 并发安全访问。
	consoleSessions sync.Map

	// streamMu 序列化对 gRPC 客户端流的并发 Send 调用。
	// gRPC Go 客户端的 Send 内部可能无锁，控制台高频输出必须显式加锁。
	streamMu sync.Mutex
}

func main() {
	configPath := flag.String("config", "/opt/narwhal-agent/config.json", "path to config file")
	resetPwd := flag.String("reset-password", "", "reset web panel password")
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.Printf("narwhal cloud-agent %s", version)

	// 如果指定了重置密码，更新配置并退出
	if *resetPwd != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		err = cfg.Update(func(c *config.Config) {
			c.WebPassHash = *resetPwd // config.Load 会在下次启动或本次保存时处理加密（或者我们直接在此处理）
		})
		if err != nil {
			log.Fatalf("update password: %v", err)
		}
		// 再次 Load 以触发 config.go 中的自动加密逻辑并写回
		_, _ = config.Load(*configPath)
		fmt.Println("Password updated successfully.")
		return
	}

	// 从配置文件加载所有配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	conf := cfg.Get()
	log.Printf("Config loaded from %s (virt_type=%s, web=%s)", *configPath, conf.VirtType, conf.Web)

	// 初始化数据库（存 VM 数据、流量数据等，不再存配置）
	database, dbErr := db.Init(conf.DB)
	if dbErr != nil {
		log.Fatalf("init db: %v", dbErr)
	}

	var rawMgr manager.VMManager
	switch conf.VirtType {
	case "podman":
		rawMgr, err = podman.New(database)
	case "cloudhv":
		// cloud-hypervisor 初始化时传入 IPv6 配置（从配置文件读取）
		rawMgr, err = cloudhv.New(database, conf.IPv6Mode, conf.IPv6Subnet, conf.IPv6Addr, conf.IPv6Iface)
	case "incus":
		rawMgr, err = incus.New(database, conf.IPv6Mode, conf.IPv6Subnet, conf.IPv6Addr, conf.IPv6Iface)
	default:
		log.Fatalf("unsupported virt type: %q (supported: podman, cloudhv, incus)", conf.VirtType)
	}
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	// VMService 作为服务层包装底层驱动，负责 ID 转换、托管 VM 过滤
	svc := manager.NewVMService(rawMgr, database)

	// 恢复因上次重启而中断的自定义镜像拉取（仅 podman）
	if conf.VirtType == "podman" {
		svc.ResumeCustomImagePulls()
	}

	// 虚拟化驱动自启动：agent 启动后延迟 5 秒让网络就绪，然后启动所有记录为 running 的 VM
	if conf.VirtType == "cloudhv" || conf.VirtType == "incus" {
		go func() {
			time.Sleep(5 * time.Second)
			svc.Autostart(context.Background())
		}()
	}

	hostMon := monitor.NewHostMonitor()

	pf := portforward.New(svc, database, cfg)
	// 启动时从 DB 恢复已持久化的端口转发规则
	pf.Restore(context.Background())

	// VM 创建后自动添加一条随机端口 → 22 的 SSH 转发
	svc.OnCreated = func(ctx context.Context, vmID string, bandwidthMbps int) {
		port, err := pickFreePort(20000, 60000)
		if err != nil {
			log.Printf("auto SSH portfwd: %v", err)
			return
		}
		if err := pf.AddMapping(ctx, vmID, "tcp", port, 22, "ssh"); err != nil {
			log.Printf("auto SSH portfwd %d→22 for %s: %v", port, vmID, err)
			return
		}
		log.Printf("auto SSH portfwd: %s %d→22", vmID, port)
	}

	a := &Agent{
		cfg:     cfg,
		db:      database,
		mgr:     svc,
		hostMon: hostMon,
		pf:      pf,
		config:  conf,
	}

	// 启动自动更新服务（如果检测到新版本，将在 24-72 小时内强制更新）
	upd := updater.NewService(database, version)
	go upd.Start(context.Background())

	// 启动本地 Web 状态页，供运维人员直接查看节点信息
	// 同时传入 rawMgr 以便 Web 服务能访问具体的 Manager 实现（如 CloudHV 的内存报告）
	ws := web.NewServer(database, svc, hostMon, cfg, a.pf, a, rawMgr, version, upd)
	go func() {
		log.Printf("Starting web server on %s", conf.Web)
		_ = ws.ListenAndServe(conf.Web)
	}()

	// 启动流量统计服务，定期从驱动获取流量数据并同步到数据库
	trafficSvc := traffic.NewService(svc, database, cfg)
	go trafficSvc.Start(context.Background(), 30*time.Second)

	// 启动定期清理幽灵实例服务 (每小时一次，启动时先运行一次)
	go func() {
		log.Println("[Main] Initial ghost VM cleanup at startup...")
		_ = svc.Cleanup(context.Background())

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Println("[Main] Running periodic ghost VM cleanup...")
				_ = svc.Cleanup(context.Background())
			}
		}
	}()

	// 启动时异步测速，结果只保留内存，不写配置
	go a.measureBandwidth()
	// 启动时检测公网 IPv4/IPv6，附带在心跳中上报；之后每 5 分钟刷新一次
	go a.detectIPv4()
	go a.detectIPv6()
	go a.refreshPublicIPLoop()

	// 按需启动 NDP 应答器（公网 IPv6 场景）
	if conf.NdpIface != "" {
		var cloudhvDB, incusDB interface{}
		if conf.VirtType == "cloudhv" {
			cloudhvDB = database
		}
		if conf.VirtType == "incus" {
			incusDB = database
		}
		nr, err := ndp.New(conf.NdpIface, conf.NdpSubnets, conf.NdpNetwork, podman.SocketPath, cloudhvDB, incusDB)
		if err != nil {
			log.Printf("NDP responder init error: %v", err)
		} else {
			go func() {
				if err = nr.Run(context.Background()); err != nil && !errors.Is(context.Canceled, err) {
					log.Printf("NDP responder exited: %v", err)
				}
			}()
		}
	}

	a.run()
}

// measureBandwidth 通过下载 Cloudflare 测速文件估算出口带宽，
// 结果保存到数据库并更新 HostMonitor 缓存。
func (a *Agent) measureBandwidth() {
	const testURL = "https://speed.cloudflare.com/__down?bytes=92160000"
	log.Printf("Starting bandwidth test...")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		log.Printf("Bandwidth test request error: %v", err)
		return
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Bandwidth test failed: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		log.Printf("Bandwidth test read error: %v", err)
		return
	}
	elapsed := time.Since(start).Seconds()
	mbps := int32(float64(n) * 8 / elapsed / 1_000_000)
	log.Printf("Bandwidth test result: %d Mbps (downloaded %d bytes in %.2fs)", mbps, n, elapsed)

	// 将测速结果保留在内存，HostMonitor 使用，不写配置文件
	a.hostMon.SetBandwidth(mbps)
}

// run 是主循环：持续从配置管理器读取最新配置，等待 Token 就绪后建立 gRPC 连接，
// 断连后使用指数退避自动重试（5s → 10s → 20s … 最大 5min）。
// 若本次调用曾成功建立连接，下次重试延迟重置为 baseDelay。
func (a *Agent) run() {
	const (
		baseDelay = 5 * time.Second
		maxDelay  = 5 * time.Minute
	)
	delay := baseDelay

	for {
		// 上一条连接的 sendHeartbeat 等 goroutine 可能仍在读 a.config，写入需持锁
		conf := a.cfg.Get()
		a.mu.Lock()
		a.config = conf
		a.mu.Unlock()
		if conf.Token == "" {
			a.setConnected(false, "No token configured")
			time.Sleep(10 * time.Second)
			continue
		}

		a.mu.RLock()
		beforeConnect := a.lastConnected
		a.mu.RUnlock()

		err := a.connectAndLoop()

		a.mu.RLock()
		everConnected := a.lastConnected.After(beforeConnect)
		a.mu.RUnlock()

		if everConnected {
			delay = baseDelay
		}

		// connectAndLoop 在服务端干净关流（io.EOF）时返回 nil，此处 err 可能为空
		errMsg := "stream closed by server"
		if err != nil {
			errMsg = err.Error()
		}
		log.Printf("Disconnected: %v, retrying in %v...", err, delay)
		a.setConnected(false, errMsg)
		time.Sleep(delay)

		if !everConnected {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// safeSend 线程安全地向 gRPC 流发送消息，序列化所有并发 Send 调用。
func (a *Agent) safeSend(stream agent.AgentGateway_ConnectClient, msg *agent.AgentEnvelope) error {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	return stream.Send(msg)
}

// getConsoleSession 从 consoleSessions 中查找指定 ID 的控制台会话。
func (a *Agent) getConsoleSession(sessionID string) *agentConsoleSession {
	if v, ok := a.consoleSessions.Load(sessionID); ok {
		return v.(*agentConsoleSession)
	}
	return nil
}

// handleConsoleOpen 在本地打开一个 VM 控制台 TTY 会话，
// 附接成功后持续将 TTY 输出回传给平台，直到会话结束。
func (a *Agent) handleConsoleOpen(stream agent.AgentGateway_ConnectClient, cmd *agent.CmdConsoleOpen) {
	sessCtx, cancel := context.WithCancel(context.Background())
	stdinPR, stdinPW := io.Pipe()
	resizeCh := make(chan manager.ResizeEvent, 8)

	sess := &agentConsoleSession{
		stdinCh:  make(chan []byte, 256),
		resizeCh: resizeCh,
		cancel:   cancel,
	}
	a.consoleSessions.Store(cmd.SessionId, sess)
	defer func() {
		a.consoleSessions.Delete(cmd.SessionId)
		cancel()
		_ = stdinPW.Close()
	}()

	// stdin 写入协程：串行消费 stdinCh，保证键盘输入的字节顺序。
	go func() {
		for {
			select {
			case data := <-sess.stdinCh:
				if _, err := stdinPW.Write(data); err != nil {
					return
				}
			case <-sessCtx.Done():
				return
			}
		}
	}()

	// 发送初始 resize 以设置终端尺寸（在 CONNECTED 之前）
	if cmd.Cols > 0 && cmd.Rows > 0 {
		select {
		case resizeCh <- manager.ResizeEvent{Cols: uint(cmd.Cols), Rows: uint(cmd.Rows)}:
		default:
		}
	}

	// consoleWriter 将 TTY 输出写回平台（每次 Write 对应一条 ConsoleOutput 消息）
	cw := &consoleWriter{
		sessionID: cmd.SessionId,
		agent:     a,
		stream:    stream,
	}

	// 通知平台：TTY 已成功附接
	_ = a.safeSend(stream, &agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload: &agent.AgentEnvelope_ConsoleEvent{
			ConsoleEvent: &agent.ConsoleEvent{
				SessionId: cmd.SessionId,
				Type:      agent.ConsoleEventType_CONSOLE_EVENT_CONNECTED,
			},
		},
	})

	// 阻塞直到 AttachTTY 返回（TTY 结束或 ctx 取消）
	attachErr := a.mgr.AttachTTY(sessCtx, cmd.VmId, stdinPR, cw, resizeCh)

	// 通知平台：TTY 已断开
	evtType := agent.ConsoleEventType_CONSOLE_EVENT_DISCONNECTED
	reason := ""
	if attachErr != nil && sessCtx.Err() == nil {
		evtType = agent.ConsoleEventType_CONSOLE_EVENT_ERROR
		reason = attachErr.Error()
	}
	_ = a.safeSend(stream, &agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload: &agent.AgentEnvelope_ConsoleEvent{
			ConsoleEvent: &agent.ConsoleEvent{
				SessionId: cmd.SessionId,
				Type:      evtType,
				Reason:    reason,
			},
		},
	})
}

// consoleWriter 实现 io.Writer，将 TTY 输出封装为 ConsoleOutput 消息发送给平台。
type consoleWriter struct {
	sessionID string
	agent     *Agent
	stream    agent.AgentGateway_ConnectClient
}

func (cw *consoleWriter) Write(p []byte) (int, error) {
	data := make([]byte, len(p))
	copy(data, p)
	err := cw.agent.safeSend(cw.stream, &agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload: &agent.AgentEnvelope_ConsoleOutput{
			ConsoleOutput: &agent.ConsoleOutput{
				SessionId: cw.sessionID,
				Data:      data,
			},
		},
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// setConnected 线程安全地更新连接状态
func (a *Agent) setConnected(connected bool, errMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = connected
	a.lastError = errMsg
	if connected {
		a.lastConnected = time.Now()
	}
}

// GetConnStatus 返回连接状态和错误信息
func (a *Agent) GetConnStatus() (bool, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connected, a.lastError
}

// connectAndLoop 建立 gRPC 双向流，启动心跳/端口转发上报协程，
// 然后阻塞读取平台下发的命令直到连接断开。
func (a *Agent) connectAndLoop() error {
	conn, err := grpc.NewClient(serverAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
	)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	client := agent.NewAgentGatewayClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.config.Token)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	log.Printf("Connected to platform: %s", serverAddr)
	a.setConnected(true, "")

	// 初始化 ping 监测
	a.mu.Lock()
	a.lastPingTime = time.Now()
	a.mu.Unlock()

	// 连接后立即上报一次状态，无需等待第一个 ticker
	go a.sendHeartbeat(stream)

	go a.heartbeatLoop(stream)

	// 启动 ping 超时检测：如果 60 秒未收到 Ping，判定连接死亡
	go a.monitorPingTimeout(stream.Context(), cancel)

	for {
		env, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		a.dispatch(stream, env)
	}
}

// dispatch 分发平台下发的消息：Ping / 控制台输入等控制流消息在 Recv 循环内
// 同步处理（保证顺序、避免为每条消息开 goroutine），VM 生命周期等重命令
// 交给 handleCommand 在独立 goroutine 中执行。
func (a *Agent) dispatch(stream agent.AgentGateway_ConnectClient, env *agent.PlatformEnvelope) {
	switch p := env.Payload.(type) {
	case *agent.PlatformEnvelope_Ping:
		a.mu.Lock()
		a.lastPingTime = time.Now()
		a.mu.Unlock()

	case *agent.PlatformEnvelope_ConsoleOpen:
		go a.handleConsoleOpen(stream, p.ConsoleOpen)

	case *agent.PlatformEnvelope_ConsoleInput:
		if sess := a.getConsoleSession(p.ConsoleInput.SessionId); sess != nil {
			select {
			case sess.stdinCh <- p.ConsoleInput.Data:
			default: // TTY 消费停滞时丢弃输入，避免阻塞 Recv 循环
			}
		}

	case *agent.PlatformEnvelope_ConsoleResize:
		if sess := a.getConsoleSession(p.ConsoleResize.SessionId); sess != nil {
			select {
			case sess.resizeCh <- manager.ResizeEvent{
				Cols: uint(p.ConsoleResize.Cols),
				Rows: uint(p.ConsoleResize.Rows),
			}:
			default:
			}
		}

	case *agent.PlatformEnvelope_ConsoleClose:
		if sess := a.getConsoleSession(p.ConsoleClose.SessionId); sess != nil {
			sess.cancel()
		}

	default:
		go a.handleCommand(stream, env)
	}
}

// monitorCtx 构造携带监控配置（NIC / 磁盘挂载点）的 context，
// 供 HostMonitor 按配置采集对应接口和分区的数据。
func (a *Agent) monitorCtx() context.Context {
	a.mu.RLock()
	nic, disk := a.config.MonitorNIC, a.config.MonitorDisk
	a.mu.RUnlock()
	ctx := context.WithValue(context.Background(), monitor.NICKey, nic)
	return context.WithValue(ctx, monitor.DiskKey, disk)
}

// sendHeartbeat 采集宿主机指标和容器列表，累计流量后通过 stream 上报心跳。
func (a *Agent) sendHeartbeat(stream agent.AgentGateway_ConnectClient) {
	ctx := a.monitorCtx()
	hostStats, err := a.hostMon.GetStats(ctx)
	if err != nil {
		return
	}
	hb := hostStats.Heartbeat
	hb.Timestamp = time.Now().Unix()
	a.mu.RLock()
	virtType := a.config.VirtType
	hb.VirtType = virtType
	liveHost := a.cfg.Get().Host // 每次心跳读取最新配置，确保面板修改立即生效
	if liveHost != "" {
		hb.EntryHost = liveHost
	} else {
		hb.EntryHost = a.entryIPv4 // 未手动配置时使用自动检测的公网 IPv4
	}
	hb.EntryIpv6 = a.entryIPv6
	a.mu.RUnlock()

	vms, _ := a.mgr.ListVMs(ctx)
	images, _ := a.mgr.GetSupportedImages(ctx)
	images = manager.FilterAndSortImages(a.db, images)
	if virtType == "podman" {
		// 自定义镜像随心跳上报，平台 os_images 会自动跟随更新
		images = manager.AppendReadyCustomImages(a.db, images)
	}

	// 流量数据由 TrafficService 后台定期写入 DB，这里只读取累计值填充心跳
	for _, vm := range vms {
		if t, err := a.db.GetTraffic(vm.VmId); err == nil {
			vm.TrafficInBytes = t.TotalIn
			vm.TrafficOutBytes = t.TotalOut
			vm.MonthlyTrafficIn = t.MonthIn
			vm.MonthlyTrafficOut = t.MonthOut
		}
	}
	hb.Vms = vms
	hb.OsImages = images

	_ = a.safeSend(stream, &agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload:   &agent.AgentEnvelope_Heartbeat{Heartbeat: hb},
	})
}

// heartbeatLoop 每 30 秒触发一次心跳上报，流断开时退出。
func (a *Agent) heartbeatLoop(stream agent.AgentGateway_ConnectClient) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return
		case <-ticker.C:
			a.sendHeartbeat(stream)
		}
	}
}

// handleCommand 处理平台下发的单条命令，执行完毕后通过 stream 回复结果。
// 每条命令在独立 goroutine 中执行，不阻塞主接收循环。
func (a *Agent) handleCommand(stream agent.AgentGateway_ConnectClient, env *agent.PlatformEnvelope) {
	defer func() {
		if panicErr := recover(); panicErr != nil {
			log.Printf("Panic: %v\n", panicErr)
			log.Printf("Stack:\n%s", debug.Stack())
		}
	}()
	var (
		err      error
		respData []byte
	)
	ctx := context.Background()

	switch p := env.Payload.(type) {
	case *agent.PlatformEnvelope_CreateVm:
		// VMService.CreateVM 内部已持久化 VMConfig
		err = a.mgr.CreateVM(ctx, p.CreateVm)

	case *agent.PlatformEnvelope_ReinstallVm:
		_, err = a.db.GetVMConfig(p.ReinstallVm.VmId)
		if err != nil {
			// 母鸡 DB 丢失，直接创建
			err = a.mgr.CreateVM(ctx, &agent.CmdCreateVM{
				VmId:          p.ReinstallVm.VmId,
				Cpu:           p.ReinstallVm.Cpu,
				RamMb:         p.ReinstallVm.RamMb,
				DiskGb:        p.ReinstallVm.DiskGb,
				BandwidthMbps: p.ReinstallVm.BandwidthMbps,
				OsImage:       p.ReinstallVm.OsImage,
				RootPassword:  p.ReinstallVm.RootPassword,
			})
		} else {
			// 正常重装流程
			err = a.mgr.ReinstallVM(ctx, p.ReinstallVm)
		}
		if err == nil {
			a.pf.RefreshVM(ctx, p.ReinstallVm.VmId)
		}
	case *agent.PlatformEnvelope_DeleteVm:
		err = a.mgr.DeleteVM(ctx, p.DeleteVm.VmId)
		// DeleteVM already removes VMConfig from DB; only clean up ancillary records here.
		_ = a.db.DeleteTraffic(p.DeleteVm.VmId)
		a.pf.DeleteVM(ctx, p.DeleteVm.VmId)

	case *agent.PlatformEnvelope_StartVm:
		err = a.mgr.StartVM(ctx, p.StartVm.VmId)

	case *agent.PlatformEnvelope_StopVm:
		err = a.mgr.StopVM(ctx, p.StopVm.VmId, p.StopVm.Force)

	case *agent.PlatformEnvelope_RestartVm:
		err = a.mgr.RestartVM(ctx, p.RestartVm.VmId)
		if err == nil {
			a.pf.RefreshVM(ctx, p.RestartVm.VmId)
		}

	case *agent.PlatformEnvelope_ResetPassword:
		err = a.mgr.ResetPassword(ctx, p.ResetPassword.VmId, p.ResetPassword.NewPassword)

	case *agent.PlatformEnvelope_SetPortFwd:
		proto := "tcp"
		if p.SetPortFwd.Protocol == agent.Protocol_PROTOCOL_UDP {
			proto = "udp"
		}
		err = a.pf.AddMapping(ctx, p.SetPortFwd.VmId, proto, int(p.SetPortFwd.HostPort), int(p.SetPortFwd.GuestPort), p.SetPortFwd.Description)

	case *agent.PlatformEnvelope_DelPortFwd:
		proto := "tcp"
		if p.DelPortFwd.Protocol == agent.Protocol_PROTOCOL_UDP {
			proto = "udp"
		}
		err = a.pf.RemoveMapping(ctx, p.DelPortFwd.VmId, proto, int(p.DelPortFwd.HostPort))

	case *agent.PlatformEnvelope_GetPortFwds:
		var pfList []*db.PortForward
		pfList, err = a.db.ListPortForwards(p.GetPortFwds.VmId)
		if err == nil {
			entries := make([]*agent.PortForwardEntry, 0, len(pfList))
			for _, pf := range pfList {
				proto := agent.Protocol_PROTOCOL_TCP
				if pf.Protocol == "udp" {
					proto = agent.Protocol_PROTOCOL_UDP
				}
				entries = append(entries, &agent.PortForwardEntry{
					VmId:        pf.VMID,
					Protocol:    proto,
					HostPort:    int32(pf.HostPort),
					GuestPort:   int32(pf.GuestPort),
					Description: pf.Description,
				})
			}
			_ = a.safeSend(stream, &agent.AgentEnvelope{
				MessageId: uuid.NewString(),
				Payload: &agent.AgentEnvelope_PortFwdList{
					PortFwdList: &agent.PortForwardList{
						CommandId: env.CommandId,
						Entries:   entries,
					},
				},
			})
			return // 已单独回复，跳过末尾的 CommandResult
		}
	}

	res := &agent.CommandResult{CommandId: env.CommandId, Success: err == nil}
	if err != nil {
		res.Error = err.Error()
	} else if len(respData) > 0 {
		res.Data = respData
	}
	_ = a.safeSend(stream, &agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload:   &agent.AgentEnvelope_CmdResult{CmdResult: res},
	})
}

// vmStatusString 将 proto VMStatus 枚举转为 DB 存储的状态字符串。
func vmStatusString(s agent.VMStatus) string {
	switch s {
	case agent.VMStatus_VM_STATUS_RUNNING:
		return "running"
	case agent.VMStatus_VM_STATUS_STOPPED:
		return "stopped"
	case agent.VMStatus_VM_STATUS_CREATING:
		return "creating"
	case agent.VMStatus_VM_STATUS_ERROR:
		return "error"
	default:
		return ""
	}
}

// detectIPv4 获取公网 IPv4 并更新字段；fetch 失败时保留原值。
func (a *Agent) detectIPv4() {
	ip := fetchPublicIPv4()
	if ip == "" {
		return
	}
	a.mu.Lock()
	changed := ip != a.entryIPv4
	a.entryIPv4 = ip
	a.mu.Unlock()
	if changed {
		log.Printf("Public IPv4: %s", ip)
	}
}

// fetchPublicIPv4 通过 api-ipv4.ip.sb 获取本机公网 IPv4 地址。
func fetchPublicIPv4() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api-ipv4.ip.sb/ip", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// detectIPv6 获取公网 IPv6 并更新字段；fetch 失败时保留原值。
func (a *Agent) detectIPv6() {
	ip := fetchPublicIPv6()
	if ip == "" {
		return
	}
	a.mu.Lock()
	changed := ip != a.entryIPv6
	a.entryIPv6 = ip
	a.mu.Unlock()
	if changed {
		log.Printf("Public IPv6: %s", ip)
	}
}

// refreshPublicIPLoop 每 5 分钟重新检测一次公网 IP，
// 确保母鸡 IP 变更后无需断线重连即可自动更新心跳中的入口地址。
func (a *Agent) refreshPublicIPLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		a.detectIPv4()
		a.detectIPv6()
	}
}

// fetchPublicIPv6 通过 api-ipv6.ip.sb 获取本机公网 IPv6 地址。
func fetchPublicIPv6() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api-ipv6.ip.sb/ip", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pickFreePort 在 [min, max) 范围内随机选一个当前未被占用的 TCP 端口。
func pickFreePort(min, max int) (int, error) {
	for i := 0; i < 30; i++ {
		port := min + rand.Intn(max-min)
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			_ = ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port found in [%d, %d)", min, max)
}

// monitorPingTimeout 定期检查是否长时间未收到 Ping，检测僵尸连接。
// 如果 pingTimeout (60秒) 内未收到任何 Ping，则向 pingDead 通道发送信号。
func (a *Agent) monitorPingTimeout(ctx context.Context, cancel context.CancelFunc) {
	const pingTimeout = 60 * time.Second
	const checkInterval = 10 * time.Second

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 连接因其他原因结束时退出，防止每次重连泄漏一个常驻 goroutine
			return
		case <-ticker.C:
			a.mu.RLock()
			lastPingTime := a.lastPingTime
			a.mu.RUnlock()

			if time.Since(lastPingTime) > pingTimeout {
				log.Printf("Ping timeout: no ping received for %v", pingTimeout)
				cancel()
				return
			}
		}
	}
}
