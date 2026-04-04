//go:build containers_image_openpgp

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cloudhv"
	"runman-agent/manager/podman"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/ndp"
	"runman-agent/proto/agent"
	"runman-agent/web"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

var version = "dev"

const serverAddr = "hosting.fuckip.me:443"

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
	entryIPv4     string // 公网 IPv4，启动时自动检测
	entryIPv6     string // 公网 IPv6，启动时自动检测
}

func main() {
	dbPath := flag.String("db", "agent.db", "path to sqlite db")
	webAddr := flag.String("web", ":8792", "web status server address")
	virtType := flag.String("type", "podman", "virtualization type")
	ndpIface := flag.String("ndp-iface", "", "uplink interface for NDP responder (IPv6, e.g. eth0)")
	ndpSubnets := flag.String("ndp-subnet", "", "IPv6 CIDRs for NDP responder, comma-separated (e.g. 2001:db8::/112)")
	ndpNetwork := flag.String("ndp-network", "", "Podman network name for NDP responder (e.g. narwhal-net)")
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.Printf("narwhal cloud-agent %s", version)

	database, err := db.Init(*dbPath)
	if err != nil {
		log.Fatalf("init db: %v", err)
	}

	// 首次启动时将虚拟化写入数据库
	conf, _ := database.GetConfig()
	if conf.VirtType == "" {
		conf.VirtType = *virtType
		_ = database.SaveConfig(conf)
	}

	// cloud-hypervisor: 读取 install.sh 创建的 IPv6 配置文件
	if conf.VirtType == "cloudhv" {
		ipv6ConfigPath := filepath.Join(filepath.Dir(*dbPath), "ipv6-config.json")
		if data, err := ioutil.ReadFile(ipv6ConfigPath); err == nil {
			var ipv6Cfg map[string]string
			if err := json.Unmarshal(data, &ipv6Cfg); err == nil {
				if conf.IPv6Mode == "" && ipv6Cfg["ipv6_mode"] != "" {
					conf.IPv6Mode = ipv6Cfg["ipv6_mode"]
					conf.IPv6Subnet = ipv6Cfg["ipv6_subnet"]
					conf.IPv6Addr = ipv6Cfg["ipv6_addr"]
					conf.IPv6Iface = ipv6Cfg["ipv6_iface"]
					_ = database.SaveConfig(conf)
					log.Printf("IPv6 config loaded: mode=%s", conf.IPv6Mode)
					// 删除配置文件，避免重复读取
					_ = os.Remove(ipv6ConfigPath)
				}
			}
		}
	}

	var rawMgr manager.VMManager
	switch conf.VirtType {
	case "podman":
		rawMgr, err = podman.New(database)
	case "cloudhv":
		rawMgr, err = cloudhv.New(database)
	default:
		log.Fatalf("unsupported virt type: %q (supported: podman, cloudhv)", conf.VirtType)
	}
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	// VMService 作为服务层包装底层驱动，负责 ID 转换、托管 VM 过滤
	svc := manager.NewVMService(rawMgr, database)

	// cloud-hypervisor 自启动：agent 启动后延迟 5 秒让网络就绪，然后启动所有 running VM
	if conf.VirtType == "cloudhv" {
		go func() {
			time.Sleep(5 * time.Second)
			svc.Autostart(context.Background())
		}()
	}

	hostMon := monitor.NewHostMonitor()

	pf := portforward.New(svc, database)
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
	// 启动时检测公网 IPv4，附带在心跳中上报
	go a.detectIPv4()
	// 启动时检测公网 IPv6，附带在心跳中上报
	go a.detectIPv6()
	// 独立流量采集循环，与服务端连接状态无关，保证离线时月流量也能持续统计
	go a.trafficCollectLoop()

	// 按需启动 NDP 应答器（公网 IPv6 场景）
	if *ndpIface != "" {
		var cloudhvDB interface{}
		if conf.VirtType == "cloudhv" {
			cloudhvDB = database
		}
		nr, err := ndp.New(*ndpIface, *ndpSubnets, *ndpNetwork, podman.SocketPath, cloudhvDB)
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
	defer func() { _ = resp.Body.Close() }()

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

// connectAndLoop 建立 gRPC 双向流，启动心跳/端口转发上报协程，
// 然后阻塞读取平台下发的命令直到连接断开。
func (a *Agent) connectAndLoop() error {
	conn, err := grpc.NewClient(serverAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
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

	log.Printf("Connected to platform: %s", serverAddr)
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
	hb.Timestamp = time.Now().Unix()
	hb.VirtType = a.config.VirtType
	a.mu.RLock()
	hb.EntryHost = a.config.Host
	if hb.EntryHost == "" {
		hb.EntryHost = a.entryIPv4 // 未手动配置时使用自动检测的公网 IPv4
	}
	hb.EntryIpv6 = a.entryIPv6
	a.mu.RUnlock()

	vms, _ := a.mgr.ListVMs(ctx)
	images, _ := a.mgr.GetSupportedImages(ctx)

	// 流量数据由 trafficCollectLoop 独立写入 DB，这里只读取累计值填充心跳
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
	defer func() {
		defer func() {
			if panicErr := recover(); panicErr != nil {
				log.Printf("Panic: %v\n", panicErr)
				log.Printf("Stack:\n%s", debug.Stack())
			}
		}()
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
			err = a.mgr.ReinstallVM(ctx, p.ReinstallVm)
		}
		a.pf.RefreshVM(ctx, p.ReinstallVm.VmId)
	case *agent.PlatformEnvelope_DeleteVm:
		err = a.mgr.DeleteVM(ctx, p.DeleteVm.VmId)
		_ = a.db.DeleteVMConfig(p.DeleteVm.VmId)
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

	case *agent.PlatformEnvelope_Ping:
		return

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

// detectIPv4 启动时获取一次公网 IPv4
func (a *Agent) detectIPv4() {
	ip := fetchPublicIPv4()
	a.mu.Lock()
	a.entryIPv4 = ip
	a.mu.Unlock()
	if ip != "" {
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

// detectIPv6 启动时获取一次公网 IPv6
func (a *Agent) detectIPv6() {
	ip := fetchPublicIPv6()
	a.mu.Lock()
	a.entryIPv6 = ip
	a.mu.Unlock()
	if ip != "" {
		log.Printf("Public IPv6: %s", ip)
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

// trafficCollectLoop 每 30 秒采集一次所有 VM 的流量并写入 DB，
// 独立于服务端连接状态运行，保证离线时月度流量也能持续统计。
func (a *Agent) trafficCollectLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx := a.monitorCtx()
		vms, err := a.mgr.ListVMs(ctx)
		if err != nil {
			continue
		}
		currentMonth := time.Now().Format("2006-01")
		for _, vm := range vms {
			_, _, _, _, _ = a.db.UpdateTraffic(vm.VmId, vm.TrafficInBytes, vm.TrafficOutBytes, currentMonth)
			// 同步容器状态到 DB
			if conf, dbErr := a.db.GetVMConfig(vm.VmId); dbErr == nil {
				if s := vmStatusString(vm.Status); s != "" && conf.Status != s {
					conf.Status = s
					_ = a.db.SaveVMConfig(conf)
				}
			}
		}
	}
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
