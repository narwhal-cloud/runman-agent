//go:build containers_image_openpgp

package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cpualloc"
	"runman-agent/manager/podman"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
	"runman-agent/web"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Agent 是运行在宿主机上的代理进程，负责管理容器生命周期、
// 收集监控指标并通过 gRPC 双向流与平台保持长连接。
type Agent struct {
	db      *db.DB
	mgr     manager.VMManager
	hostMon *monitor.HostMonitor
	pf      *portforward.Manager
	config  *db.Config
}

func main() {
	dbPath := flag.String("db", "agent.db", "path to sqlite db")
	webAddr := flag.String("web", ":8792", "web status server address")
	socketPath := flag.String("socket", "unix:///run/podman/podman.sock", "podman api socket path")
	virtType := flag.String("type", "podman", "virtualization type")
	flag.Parse()

	database, err := db.Init(*dbPath)
	if err != nil {
		log.Fatalf("init db: %v", err)
	}

	// 首次启动时将命令行指定的虚拟化类型写入数据库
	conf, _ := database.GetConfig()
	if conf.VirtType == "" {
		conf.VirtType = *virtType
		_ = database.SaveConfig(conf)
	}

	var rawMgr manager.VMManager
	if conf.VirtType == "podman" {
		rawMgr, err = podman.New(*socketPath)
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

	a := &Agent{
		db:      database,
		mgr:     svc,
		hostMon: hostMon,
		pf:      portforward.New(svc),
		config:  conf,
	}

	// 启动本地 Web 状态页，供运维人员直接查看节点信息
	ws := web.NewServer(database, svc, hostMon)
	go func() {
		log.Printf("Starting web server on %s", *webAddr)
		_ = ws.ListenAndServe(*webAddr)
	}()

	// 启动时异步测速，结果写入 DB 并附带在心跳中上报
	go a.measureBandwidth()

	a.run()
}

// measureBandwidth 通过下载 Cloudflare 测速文件估算出口带宽，
// 结果保存到数据库并更新 HostMonitor 缓存。
func (a *Agent) measureBandwidth() {
	const testURL = "https://speed.cloudflare.com/__down?bytes=10240000"
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
			time.Sleep(10 * time.Second)
			continue
		}
		err := a.connectAndLoop()
		log.Printf("Disconnected: %v, retrying in 5s...", err)
		time.Sleep(5 * time.Second)
	}
}

// connectAndLoop 建立 gRPC 双向流，启动心跳/端口转发上报协程，
// 然后阻塞读取平台下发的命令直到连接断开。
func (a *Agent) connectAndLoop() error {
	conn, err := grpc.NewClient(a.config.ServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	// 连接后立即上报一次状态，无需等待第一个 ticker
	go a.sendHeartbeat(stream)
	go a.sendPortForwardReport(stream)

	go a.heartbeatLoop(stream)
	go a.portForwardReportLoop(stream)

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

	vms, _ := a.mgr.ListVMs(ctx)
	images, _ := a.mgr.GetSupportedImages(ctx)

	// 将本次采集到的流量增量累加到 DB，并写回累计值及当月统计
	currentMonth := time.Now().Format("2006-01")
	for _, vm := range vms {
		totalIn, totalOut, monthIn, monthOut, _ := a.db.UpdateTraffic(vm.VmId, vm.TrafficInBytes, vm.TrafficOutBytes, currentMonth)
		vm.TrafficInBytes, vm.TrafficOutBytes = totalIn, totalOut
		vm.MonthlyTrafficIn, vm.MonthlyTrafficOut = monthIn, monthOut
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

// portForwardReportLoop 每 5 分钟上报当前所有端口转发规则，流断开时退出。
func (a *Agent) portForwardReportLoop(stream agent.AgentGateway_ConnectClient) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return
		case <-ticker.C:
			a.sendPortForwardReport(stream)
		}
	}
}

// sendPortForwardReport 将内存中所有活跃的端口转发规则上报给平台。
func (a *Agent) sendPortForwardReport(stream agent.AgentGateway_ConnectClient) {
	mappings := a.pf.GetReport()
	var entries []*agent.PortForwardEntry
	for _, m := range mappings {
		protocol := agent.Protocol_PROTOCOL_TCP
		if m.Protocol == "udp" {
			protocol = agent.Protocol_PROTOCOL_UDP
		}
		entries = append(entries, &agent.PortForwardEntry{
			VmId: m.VMID, Protocol: protocol, HostPort: int32(m.HostPort), GuestPort: int32(m.GuestPort),
		})
	}
	_ = stream.Send(&agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload:   &agent.AgentEnvelope_PortFwdReport{PortFwdReport: &agent.PortForwardReport{Entries: entries}},
	})
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
		err = a.mgr.CreateVM(ctx, p.CreateVm)
		if err == nil {
			// 创建成功后将配置持久化，供端口转发限速等功能读取。
			// LocalID 记录底层虚拟化实际使用的标识符（podman 下为容器名，与 VmId 相同）。
			// Cpuset 由 VMService.CreateVM 分配并注入 ctx，此处读回持久化。
			_ = a.db.SaveVMConfig(&db.VMConfig{
				VMID:          p.CreateVm.VmId,
				LocalID:       p.CreateVm.VmId,
				BandwidthMbps: int(p.CreateVm.BandwidthMbps),
				CPU:           int(p.CreateVm.Cpu),
				MemoryMB:      p.CreateVm.RamMb,
				Image:         p.CreateVm.OsImage,
				Cpuset:        manager.CpusetFrom(ctx),
			})
		}

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
			// 重装后更新镜像记录
			if conf, _ := a.db.GetVMConfig(p.ReinstallVm.VmId); conf != nil {
				conf.Image = p.ReinstallVm.OsImage
				_ = a.db.SaveVMConfig(conf)
			}
		}

	case *agent.PlatformEnvelope_DeleteVm:
		err = a.mgr.DeleteVM(ctx, p.DeleteVm.VmId)
		// 无论删除是否成功都清理 DB 记录，避免残留脏数据
		_ = a.db.DeleteVMConfig(p.DeleteVm.VmId)

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
		err = a.pf.AddMapping(ctx, p.SetPortFwd.VmId, proto, int(p.SetPortFwd.HostPort), int(p.SetPortFwd.GuestPort), mbps)

	case *agent.PlatformEnvelope_DelPortFwd:
		proto := "tcp"
		if p.DelPortFwd.Protocol == agent.Protocol_PROTOCOL_UDP {
			proto = "udp"
		}
		err = a.pf.RemoveMapping(ctx, p.DelPortFwd.VmId, proto, int(p.DelPortFwd.HostPort))

	case *agent.PlatformEnvelope_SyncPortFwds:
		// 以平台下发的规则列表为准，全量对齐本地转发状态
		mbps := 0
		if conf, _ := a.db.GetVMConfig(p.SyncPortFwds.VmId); conf != nil {
			mbps = conf.BandwidthMbps
		}
		var desired []portforward.DesiredRule
		for _, e := range p.SyncPortFwds.Rules {
			proto := "tcp"
			if e.Protocol == agent.Protocol_PROTOCOL_UDP {
				proto = "udp"
			}
			desired = append(desired, portforward.DesiredRule{
				Protocol:  proto,
				HostPort:  int(e.HostPort),
				GuestPort: int(e.GuestPort),
			})
		}
		err = a.pf.SyncForVM(ctx, p.SyncPortFwds.VmId, desired, mbps)
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
