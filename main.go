//go:build containers_image_openpgp

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cloudhv"
	"runman-agent/manager/cpualloc"
	"runman-agent/manager/podman"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/ndp"
	"runman-agent/proto/agent"
	"runman-agent/web"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

var version = "dev"

// Agent 是运行在宿主机上的代理进程，负责管理容器生命周期、
// 收集监控指标并通过 gRPC 双向流与平台保持长连接。
type Agent struct {
	db            *db.DB
	mgr           manager.VMManager
	hostMon       *monitor.HostMonitor
	pf            *portforward.Manager
	config        *db.Config
	mu            sync.RWMutex
	connected     bool
	lastError     string
	lastConnected time.Time
}

func main() {
	dbPath := flag.String("db", "agent.db", "path to sqlite db")
	webAddr := flag.String("web", ":8792", "web status server address")
	socketPath := flag.String("socket", "unix:///run/podman/podman.sock", "podman api socket path")
	virtType := flag.String("type", "podman", "virtualization type")
	serverAddr := flag.String("server", "", "platform gRPC address (write to DB only when DB is empty)")
	token := flag.String("token", "", "agent token (write to DB only when DB is empty)")
	webUser := flag.String("web-user", "", "web panel username (write to DB only when DB is empty)")
	webPass := flag.String("web-pass", "", "web panel password in plaintext (bcrypt-hashed before storing)")
	ndpIface := flag.String("ndp-iface", "", "uplink interface for NDP responder (IPv6, e.g. eth0)")
	ndpSubnets := flag.String("ndp-subnet", "", "IPv6 CIDRs for NDP responder, comma-separated (e.g. 2001:db8::/112)")
	ndpNetwork := flag.String("ndp-network", "", "Podman network name for NDP responder (e.g. narwhal-net)")
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.Printf("runman-agent %s", version)

	database, err := db.Init(*dbPath)
	if err != nil {
		log.Fatalf("init db: %v", err)
	}

	// 首次启动时将命令行指定的虚拟化类型写入数据库；
	// 若提供了 --server / --token 则直接覆盖 DB（方便脚本一次性配置）。
	conf, _ := database.GetConfig()
	changed := false
	if conf.VirtType == "" {
		conf.VirtType = *virtType
		changed = true
	}
	if *serverAddr != "" && conf.ServerAddr == "" {
		conf.ServerAddr = *serverAddr
		changed = true
	}
	if *token != "" && conf.Token == "" {
		conf.Token = *token
		changed = true
	}
	if *webUser != "" && conf.WebUser == "" {
		conf.WebUser = *webUser
		changed = true
	}
	if *webPass != "" && conf.WebPassHash == "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*webPass), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("hash web password: %v", err)
		}
		conf.WebPassHash = string(hash)
		changed = true
	}
	if changed {
		_ = database.SaveConfig(conf)
	}

	var rawMgr manager.VMManager
	switch conf.VirtType {
	case "podman":
		rawMgr, err = podman.New(*socketPath)
	case "cloudhv":
		rawMgr, err = cloudhv.New(*socketPath)
	default:
		log.Fatalf("unsupported virt type: %q (supported: podman, cloudhv)", conf.VirtType)
	}
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	// 初始化 CPU 分配器，从 DB 恢复已有容器的 cpuset 引用计数
	alloc := cpualloc.New(cpualloc.HostCPUCount())
	if vmConfigs, err2 := database.ListVMConfigs(); err2 == nil {
		for _, c := range vmConfigs {
			alloc.Restore(c.Cpuset)
		}
	}

	// VMService 作为服务层包装底层驱动，负责 ID 转换、托管 VM 过滤和 CPU 分配
	svc := manager.NewVMService(rawMgr, database, alloc)

	hostMon := monitor.NewHostMonitor()

	pf := portforward.New(svc, database)
	// 启动时从 DB 恢复已持久化的端口转发规则
	pf.Restore(context.Background())

	a := &Agent{
		db:      database,
		mgr:     svc,
		hostMon: hostMon,
		pf:      pf,
		config:  conf,
	}

	// 启动本地 Web 状态页，供运维人员直接查看节点信息
	ws := web.NewServer(database, svc, hostMon, a.pf, a)
	go func() {
		log.Printf("Starting web server on %s", *webAddr)
		_ = ws.ListenAndServe(*webAddr)
	}()

	// 启动时异步测速，结果写入 DB 并附带在心跳中上报
	go a.measureBandwidth()

	// 按需启动 NDP 应答器（公网 IPv6 场景）
	if *ndpIface != "" {
		nr, err := ndp.New(*ndpIface, *ndpSubnets, *ndpNetwork, *socketPath)
		if err != nil {
			log.Printf("NDP responder init error: %v", err)
		} else {
			go func() {
				if err := nr.Run(context.Background()); err != nil && err != context.Canceled {
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
	const testURL = "https://speed.cloudflare.com/__down?bytes=40960000"
	log.Printf("Starting bandwidth test...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	defer resp.Body.Close()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		log.Printf("Bandwidth test read error: %v", err)
		return
	}
	elapsed := time.Since(start).Seconds()
	mbps := int32(float64(n) * 8 / elapsed / 1_000_000)
	log.Printf("Bandwidth test result: %d Mbps (downloaded %d bytes in %.2fs)", mbps, n, elapsed)

	a.hostMon.SetBandwidth(mbps)

	conf, _ := a.db.GetConfig()
	if conf != nil {
		conf.BandwidthMbps = mbps
		_ = a.db.SaveConfig(conf)
	}
}

// run 是主循环：持续从 DB 读取最新配置，等待 Token 就绪后建立 gRPC 连接，
// 断连后自动重试。
func (a *Agent) run() {
	for {
		conf, _ := a.db.GetConfig()
		a.config = conf
		if a.config.Token == "" {
			log.Printf("Waiting for configuration...")
			a.setConnected(false, "No token configured")
			time.Sleep(10 * time.Second)
			continue
		}
		err := a.connectAndLoop()
		log.Printf("Disconnected: %v, retrying in 5s...", err)
		a.setConnected(false, err.Error())
		time.Sleep(5 * time.Second)
	}
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

// isSecureConnection 判断是否需要 TLS
// 规则：如果端口是 443 或 8443，或者没有显式指定端口但域名看起来是生产环境，使用 TLS
func (a *Agent) isSecureConnection() bool {
	addr := a.config.ServerAddr
	// 如果是 :443 或 :8443，需要 TLS
	if strings.HasSuffix(addr, ":443") || strings.HasSuffix(addr, ":8443") {
		return true
	}
	// 如果没有端口号，且不是 localhost/127.0.0.1，默认使用 TLS（生产环境）
	if !strings.Contains(addr, ":") && !strings.Contains(addr, "localhost") && !strings.Contains(addr, "127.0.0.1") {
		return true
	}
	return false
}

// connectAndLoop 建立 gRPC 双向流，启动心跳/端口转发上报协程，
// 然后阻塞读取平台下发的命令直到连接断开。
func (a *Agent) connectAndLoop() error {
	// 根据连接地址判断是否需要 TLS
	var dialOpt grpc.DialOption
	if a.isSecureConnection() {
		// 使用 TLS（标准证书验证）
		dialOpt = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))
	} else {
		// 本地开发或内网环境，使用 insecure
		dialOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	conn, err := grpc.NewClient(a.config.ServerAddr, dialOpt)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	client := agent.NewAgentGatewayClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+a.config.Token)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	log.Printf("Connected to platform: %s", a.config.ServerAddr)
	a.setConnected(true, "")

	// 连接后立即上报一次状态，无需等待第一个 ticker
	go a.sendHeartbeat(stream)

	go a.heartbeatLoop(stream)

	for {
		env, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		go a.handleCommand(stream, env)
	}
}

// monitorCtx 构造携带监控配置（NIC / 磁盘挂载点）的 context，
// 供 HostMonitor 按配置采集对应接口和分区的数据。
func (a *Agent) monitorCtx() context.Context {
	ctx := context.WithValue(context.Background(), monitor.NICKey, a.config.MonitorNIC)
	return context.WithValue(ctx, monitor.DiskKey, a.config.MonitorDisk)
}

// sendHeartbeat 采集宿主机指标和容器列表，累计流量后通过 stream 上报心跳。
func (a *Agent) sendHeartbeat(stream agent.AgentGateway_ConnectClient) {
	ctx := a.monitorCtx()
	hostStats, err := a.hostMon.GetStats(ctx)
	if err != nil {
		return
	}
	hb := hostStats.Heartbeat
	hb.VirtType = a.config.VirtType

	vms, _ := a.mgr.ListVMs(ctx)
	images, _ := a.mgr.GetSupportedImages(ctx)

	// 将本次采集到的流量增量累加到 DB，并写回累计值及当月统计；同步容器真实状态
	currentMonth := time.Now().Format("2006-01")
	for _, vm := range vms {
		totalIn, totalOut, monthIn, monthOut, _ := a.db.UpdateTraffic(vm.VmId, vm.TrafficInBytes, vm.TrafficOutBytes, currentMonth)
		vm.TrafficInBytes, vm.TrafficOutBytes = totalIn, totalOut
		vm.MonthlyTrafficIn, vm.MonthlyTrafficOut = monthIn, monthOut

		if conf, err := a.db.GetVMConfig(vm.VmId); err == nil {
			if s := vmStatusString(vm.Status); s != "" && conf.Status != s {
				conf.Status = s
				_ = a.db.SaveVMConfig(conf)
			}
		}
	}
	hb.Vms = vms
	hb.OsImages = images

	_ = stream.Send(&agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload:   &agent.AgentEnvelope_Heartbeat{Heartbeat: hb},
	})
}

// heartbeatLoop 每 30 秒触发一次心跳上报，流断开时退出。
func (a *Agent) heartbeatLoop(stream agent.AgentGateway_ConnectClient) {
	ticker := time.NewTicker(30 * time.Second)
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
	var (
		err      error
		respData []byte
	)
	ctx := context.Background()

	switch p := env.Payload.(type) {
	case *agent.PlatformEnvelope_CreateVm:
		// VMService.CreateVM 内部已持久化 VMConfig（含 cpuset）
		err = a.mgr.CreateVM(ctx, p.CreateVm)

	case *agent.PlatformEnvelope_UpdateVm:
		err = a.mgr.UpdateVM(ctx, p.UpdateVm)
		if err == nil {
			if conf, _ := a.db.GetVMConfig(p.UpdateVm.VmId); conf != nil {
				// 只更新非零字段，保留原有值
				if p.UpdateVm.Cpu > 0 {
					conf.CPU = int(p.UpdateVm.Cpu)
				}
				if p.UpdateVm.RamMb > 0 {
					conf.MemoryMB = p.UpdateVm.RamMb
				}
				if p.UpdateVm.BandwidthMbps > 0 {
					conf.BandwidthMbps = int(p.UpdateVm.BandwidthMbps)
					// 同步更新端口转发限速
					a.pf.UpdateVMBandwidth(ctx, p.UpdateVm.VmId, int(p.UpdateVm.BandwidthMbps))
				}
				_ = a.db.SaveVMConfig(conf)
			}
		}

	case *agent.PlatformEnvelope_ReinstallVm:
		err = a.mgr.ReinstallVM(ctx, p.ReinstallVm)
		if err == nil {
			// 重装后更新配置记录；若 DB 中无记录（母鸡重装系统后配置丢失），则根据下发参数重新创建
			conf, _ := a.db.GetVMConfig(p.ReinstallVm.VmId)
			if conf == nil {
				conf = &db.VMConfig{
					VMID:    p.ReinstallVm.VmId,
					LocalID: p.ReinstallVm.VmId,
				}
			}
			conf.Image = p.ReinstallVm.OsImage
			if p.ReinstallVm.Cpu > 0 {
				conf.CPU = int(p.ReinstallVm.Cpu)
			}
			if p.ReinstallVm.RamMb > 0 {
				conf.MemoryMB = p.ReinstallVm.RamMb
			}
			if p.ReinstallVm.BandwidthMbps > 0 {
				conf.BandwidthMbps = int(p.ReinstallVm.BandwidthMbps)
				a.pf.UpdateVMBandwidth(ctx, p.ReinstallVm.VmId, int(p.ReinstallVm.BandwidthMbps))
			}
			// 重装后获取新的 IP，如果获取不到 MAC 就从 IP 生成
			ip, _ := a.mgr.GetVMIP(ctx, p.ReinstallVm.VmId)
			mac, _ := a.mgr.GetVMMAC(ctx, p.ReinstallVm.VmId)
			if mac == "" && ip != "" {
				mac = manager.GenerateMACFromIP(ip)
			}
			conf.IP = ip
			conf.MAC = mac
			_ = a.db.SaveVMConfig(conf)
		}

	case *agent.PlatformEnvelope_DeleteVm:
		err = a.mgr.DeleteVM(ctx, p.DeleteVm.VmId)
		// 无论删除是否成功都清理 DB 记录，避免残留脏数据
		_ = a.db.DeleteVMConfig(p.DeleteVm.VmId)
		a.pf.DeleteVM(ctx, p.DeleteVm.VmId)

	case *agent.PlatformEnvelope_StartVm:
		err = a.mgr.StartVM(ctx, p.StartVm.VmId)

	case *agent.PlatformEnvelope_StopVm:
		err = a.mgr.StopVM(ctx, p.StopVm.VmId, p.StopVm.Force)

	case *agent.PlatformEnvelope_RestartVm:
		err = a.mgr.RestartVM(ctx, p.RestartVm.VmId)

	case *agent.PlatformEnvelope_ResetPassword:
		err = a.mgr.ResetPassword(ctx, p.ResetPassword.VmId, p.ResetPassword.NewPassword)

	case *agent.PlatformEnvelope_GetVmInfo:
		// 查询结果以 JSON 编码写入 CommandResult.Data 返回给平台
		var info *agent.VMSummary
		info, err = a.mgr.GetVMInfo(ctx, p.GetVmInfo.VmId)
		if err == nil {
			respData, _ = json.Marshal(info)
		}

	case *agent.PlatformEnvelope_SetPortFwd:
		proto := "tcp"
		if p.SetPortFwd.Protocol == agent.Protocol_PROTOCOL_UDP {
			proto = "udp"
		}
		// 从 DB 读取该 VM 的带宽配置用于限速
		mbps := 0
		if conf, _ := a.db.GetVMConfig(p.SetPortFwd.VmId); conf != nil {
			mbps = conf.BandwidthMbps
		}
		err = a.pf.AddMapping(ctx, p.SetPortFwd.VmId, proto, int(p.SetPortFwd.HostPort), int(p.SetPortFwd.GuestPort), mbps, p.SetPortFwd.Description)

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
			_ = stream.Send(&agent.AgentEnvelope{
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
	_ = stream.Send(&agent.AgentEnvelope{
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
