package main

import (
	"context"
	"flag"
	"io"
	"log"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
	"runman-agent/web"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type Agent struct {
	db      *db.DB
	mgr     manager.VMManager
	hostMon *monitor.HostMonitor
	pf      *manager.PortForwardManager
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

	conf, _ := database.GetConfig()
	if conf.VirtType == "" {
		conf.VirtType = *virtType
		_ = database.SaveConfig(conf)
	}

	var mgr manager.VMManager
	if conf.VirtType == "podman" {
		mgr, err = manager.NewPodmanManager(*socketPath)
	}
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	hostMon := monitor.NewHostMonitor()

	a := &Agent{
		db:      database,
		mgr:     mgr,
		hostMon: hostMon,
		pf:      manager.NewPortForwardManager(mgr),
		config:  conf,
	}

	ws := web.NewServer(database, mgr, hostMon)
	go func() {
		log.Printf("Starting web server on %s", *webAddr)
		_ = ws.ListenAndServe(*webAddr)
	}()

	a.run()
}

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

func (a *Agent) heartbeatLoop(stream agent.AgentGateway_ConnectClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return
		case <-ticker.C:
			ctx := context.WithValue(context.Background(), "monitor_nic", a.config.MonitorNIC)
			ctx = context.WithValue(ctx, "monitor_disk", a.config.MonitorDisk)

			// 1. 获取宿主机状态 (独立模块)
			hostStats, err := a.hostMon.GetStats(ctx)
			if err != nil {
				continue
			}
			hb := hostStats.Heartbeat

			// 2. 获取 VM 状态与支持镜像 (虚拟化模块)
			vms, _ := a.mgr.ListVMs(ctx)
			images, _ := a.mgr.GetSupportedImages(ctx)

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
	}
}

func (a *Agent) portForwardReportLoop(stream agent.AgentGateway_ConnectClient) {
	ticker := time.NewTicker(5 * time.Minute)
	for {
		select {
		case <-stream.Context().Done():
			return
		case <-ticker.C:
			a.sendPortForwardReport(stream)
		}
	}
}

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

func (a *Agent) handleCommand(stream agent.AgentGateway_ConnectClient, env *agent.PlatformEnvelope) {
	var err error
	ctx := context.Background()
	switch p := env.Payload.(type) {
	case *agent.PlatformEnvelope_CreateVm:
		err = a.mgr.CreateVM(ctx, p.CreateVm)
		if err == nil {
			_ = a.db.SaveVMConfig(&db.VMConfig{
				VMID: p.CreateVm.VmId, BandwidthMbps: int(p.CreateVm.BandwidthMbps),
				CPU: int(p.CreateVm.Cpu), MemoryMB: p.CreateVm.RamMb, Image: p.CreateVm.OsImage,
			})
		}
	case *agent.PlatformEnvelope_UpdateVm:
		err = a.mgr.UpdateVM(ctx, p.UpdateVm)
	case *agent.PlatformEnvelope_DeleteVm:
		err = a.mgr.DeleteVM(ctx, p.DeleteVm.VmId)
		_ = a.db.DeleteVMConfig(p.DeleteVm.VmId)
	case *agent.PlatformEnvelope_StartVm:
		err = a.mgr.StartVM(ctx, p.StartVm.VmId)
	case *agent.PlatformEnvelope_StopVm:
		err = a.mgr.StopVM(ctx, p.StopVm.VmId, p.StopVm.Force)
	case *agent.PlatformEnvelope_RestartVm:
		err = a.mgr.RestartVM(ctx, p.RestartVm.VmId)
	case *agent.PlatformEnvelope_ResetPassword:
		err = a.mgr.ResetPassword(ctx, p.ResetPassword.VmId, p.ResetPassword.NewPassword)
	case *agent.PlatformEnvelope_SetPortFwd:
		proto := "tcp"
		if p.SetPortFwd.Protocol == agent.Protocol_PROTOCOL_UDP {
			proto = "udp"
		}
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
	}
	res := &agent.CommandResult{CommandId: env.CommandId, Success: err == nil}
	if err != nil {
		res.Error = err.Error()
	}
	_ = stream.Send(&agent.AgentEnvelope{
		MessageId: uuid.NewString(),
		Payload:   &agent.AgentEnvelope_CmdResult{CmdResult: res},
	})
}
