package web

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/disk"
	psnet "github.com/shirou/gopsutil/v4/net"
)

//go:embed static/*
var staticFiles embed.FS

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	db      *db.DB
	mgr     manager.VMManager
	hostMon *monitor.HostMonitor
	pf      *portforward.Manager
}

func NewServer(database *db.DB, mgr manager.VMManager, hostMon *monitor.HostMonitor, pf *portforward.Manager) *Server {
	return &Server{
		db:      database,
		mgr:     mgr,
		hostMon: hostMon,
		pf:      pf,
	}
}

func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()

	// 系统
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/system/info", s.handleSystemInfo)

	// 镜像列表
	mux.HandleFunc("GET /api/images", s.handleImages)

	// VM 集合
	mux.HandleFunc("GET /api/vms", s.handleListVMs)
	mux.HandleFunc("POST /api/vms", s.handleCreateVM)

	// VM 单机操作
	mux.HandleFunc("GET /api/vms/{id}", s.handleGetVM)
	mux.HandleFunc("PATCH /api/vms/{id}", s.handleUpdateVM)
	mux.HandleFunc("POST /api/vms/{id}/start", s.handleStartVM)
	mux.HandleFunc("POST /api/vms/{id}/stop", s.handleStopVM)
	mux.HandleFunc("POST /api/vms/{id}/restart", s.handleRestartVM)
	mux.HandleFunc("DELETE /api/vms/{id}", s.handleDeleteVM)
	mux.HandleFunc("POST /api/vms/{id}/reinstall", s.handleReinstallVM)
	mux.HandleFunc("POST /api/vms/{id}/reset-password", s.handleResetPassword)

	// 端口转发
	mux.HandleFunc("GET /api/vms/{id}/portfwds", s.handleListPortFwds)
	mux.HandleFunc("POST /api/vms/{id}/portfwds", s.handleAddPortFwd)
	mux.HandleFunc("PUT /api/vms/{id}/portfwds", s.handleSyncPortFwds)
	mux.HandleFunc("DELETE /api/vms/{id}/portfwds/{protocol}/{hostPort}", s.handleDelPortFwd)

	// 控制台 TTY（WebSocket）
	mux.HandleFunc("GET /api/vms/{id}/tty", s.handleVMTTY)

	// 静态文件
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, _ := staticFiles.ReadFile("static/index.html")
			w.Header().Set("Content-Type", "text/html")
			w.Write(data)
			return
		}
		filePath := "static" + r.URL.Path
		data, err := staticFiles.ReadFile(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(filePath, ".png") {
			w.Header().Set("Content-Type", "image/png")
		}
		w.Write(data)
	})

	return http.ListenAndServe(addr, mux)
}

// ─── 辅助 ──────────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
}

// ─── 系统 ──────────────────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conf, _ := s.db.GetConfig()
	ctx := context.WithValue(r.Context(), monitor.NICKey, conf.MonitorNIC)
	ctx = context.WithValue(ctx, monitor.DiskKey, conf.MonitorDisk)

	stats, err := s.hostMon.GetStats(ctx)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, stats)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	nics, _ := psnet.Interfaces()
	parts, _ := disk.Partitions(false)

	var nicNames []string
	for _, n := range nics {
		nicNames = append(nicNames, n.Name)
	}
	var mountPoints []string
	for _, p := range parts {
		mountPoints = append(mountPoints, p.Mountpoint)
	}
	jsonOK(w, map[string][]string{"nics": nicNames, "disks": mountPoints})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req db.Config
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		existing, _ := s.db.GetConfig()
		if existing != nil {
			existing.ServerAddr = req.ServerAddr
			existing.Token = req.Token
			existing.MonitorNIC = req.MonitorNIC
			existing.MonitorDisk = req.MonitorDisk
			s.db.SaveConfig(existing)
		}
		w.WriteHeader(200)
		return
	}
	conf, _ := s.db.GetConfig()
	jsonOK(w, conf)
}

// ─── 镜像 ──────────────────────────────────────────────────────────────────────

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.mgr.GetSupportedImages(r.Context())
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, images)
}

// ─── VM 列表 & 创建 ────────────────────────────────────────────────────────────

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.mgr.ListVMs(r.Context())

	type vmItem struct {
		*agent.VMSummary
		RamTotalMb int64 `json:"ram_total_mb,omitempty"`
	}
	items := make([]vmItem, len(vms))
	for i, vm := range vms {
		item := vmItem{VMSummary: vm}
		if conf, _ := s.db.GetVMConfig(vm.VmId); conf != nil {
			item.RamTotalMb = conf.MemoryMB
		}
		items[i] = item
	}
	jsonOK(w, items)
}

// createVMRequest 对应 POST /api/vms 请求体。
// VmId 可选，缺省时自动生成 UUID。
type createVMRequest struct {
	VmId          string `json:"vm_id"`
	Cpu           int32  `json:"cpu"`
	RamMb         int64  `json:"ram_mb"`
	DiskGb        int64  `json:"disk_gb"`
	BandwidthMbps int32  `json:"bandwidth_mbps"`
	OsImage       string `json:"os_image"`
	RootPassword  string `json:"root_password"`
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req createVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.OsImage == "" || req.RootPassword == "" {
		http.Error(w, "os_image and root_password are required", 400)
		return
	}
	if req.VmId == "" {
		req.VmId = uuid.NewString()
	}

	cmd := &agent.CmdCreateVM{
		VmId:          req.VmId,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
	}

	ctx := r.Context()
	if err := s.mgr.CreateVM(ctx, cmd); err != nil {
		jsonErr(w, err, 500)
		return
	}

	// VMService.CreateVM 内部已持久化 VMConfig（含 cpuset）

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"vm_id": req.VmId})
}

// ─── VM 单机操作 ───────────────────────────────────────────────────────────────

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	info, err := s.mgr.GetVMInfo(r.Context(), vmID)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, info)
}

// updateVMRequest 对应 PATCH /api/vms/{id} 请求体，零值字段表示不修改。
type updateVMRequest struct {
	Cpu           int32 `json:"cpu"`
	RamMb         int64 `json:"ram_mb"`
	DiskGb        int64 `json:"disk_gb"`
	BandwidthMbps int32 `json:"bandwidth_mbps"`
}

func (s *Server) handleUpdateVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req updateVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	cmd := &agent.CmdUpdateVM{
		VmId:          vmID,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	}

	ctx := r.Context()
	if err := s.mgr.UpdateVM(ctx, cmd); err != nil {
		jsonErr(w, err, 500)
		return
	}

	// 更新 DB 中的 VMConfig
	if conf, _ := s.db.GetVMConfig(vmID); conf != nil {
		if req.Cpu > 0 {
			conf.CPU = int(req.Cpu)
		}
		if req.RamMb > 0 {
			conf.MemoryMB = req.RamMb
		}
		if req.BandwidthMbps > 0 {
			conf.BandwidthMbps = int(req.BandwidthMbps)
			s.pf.UpdateVMBandwidth(ctx, vmID, int(req.BandwidthMbps))
		}
		_ = s.db.SaveVMConfig(conf)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.StartVM(r.Context(), r.PathValue("id")); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.mgr.StopVM(r.Context(), r.PathValue("id"), req.Force); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestartVM(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RestartVM(r.Context(), r.PathValue("id")); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	if err := s.mgr.DeleteVM(r.Context(), vmID); err != nil {
		jsonErr(w, err, 500)
		return
	}
	_ = s.db.DeleteVMConfig(vmID)
	w.WriteHeader(http.StatusNoContent)
}

// reinstallVMRequest 对应 POST /api/vms/{id}/reinstall 请求体。
type reinstallVMRequest struct {
	OsImage       string `json:"os_image"`
	RootPassword  string `json:"root_password"`
	Cpu           int32  `json:"cpu"`
	RamMb         int64  `json:"ram_mb"`
	DiskGb        int64  `json:"disk_gb"`
	BandwidthMbps int32  `json:"bandwidth_mbps"`
}

func (s *Server) handleReinstallVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req reinstallVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.OsImage == "" || req.RootPassword == "" {
		http.Error(w, "os_image and root_password are required", 400)
		return
	}

	cmd := &agent.CmdReinstallVM{
		VmId:          vmID,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	}

	if err := s.mgr.ReinstallVM(r.Context(), cmd); err != nil {
		jsonErr(w, err, 500)
		return
	}

	// 更新 DB 中的 VMConfig
	conf, _ := s.db.GetVMConfig(vmID)
	if conf == nil {
		conf = &db.VMConfig{VMID: vmID, LocalID: vmID}
	}
	conf.Image = req.OsImage
	if req.Cpu > 0 {
		conf.CPU = int(req.Cpu)
	}
	if req.RamMb > 0 {
		conf.MemoryMB = req.RamMb
	}
	if req.BandwidthMbps > 0 {
		conf.BandwidthMbps = int(req.BandwidthMbps)
	}
	_ = s.db.SaveVMConfig(conf)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		http.Error(w, "missing password", 400)
		return
	}
	if err := s.mgr.ResetPassword(r.Context(), vmID, req.Password); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── 端口转发 ──────────────────────────────────────────────────────────────────

// portFwdEntry 是端口转发规则的 JSON 表示。
type portFwdEntry struct {
	Protocol    string `json:"protocol"`
	HostPort    int    `json:"host_port"`
	GuestPort   int    `json:"guest_port"`
	Description string `json:"description,omitempty"`
}

func (s *Server) handleListPortFwds(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	all := s.pf.GetReport()
	result := make([]portFwdEntry, 0)
	for _, e := range all {
		if e.VMID == vmID {
			result = append(result, portFwdEntry{
				Protocol:    e.Protocol,
				HostPort:    e.HostPort,
				GuestPort:   e.GuestPort,
				Description: e.Description,
			})
		}
	}
	jsonOK(w, result)
}

func (s *Server) handleAddPortFwd(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req portFwdEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" {
		http.Error(w, "protocol must be tcp or udp", 400)
		return
	}
	if req.HostPort <= 0 || req.GuestPort <= 0 {
		http.Error(w, "host_port and guest_port must be positive", 400)
		return
	}

	mbps := 0
	if conf, _ := s.db.GetVMConfig(vmID); conf != nil {
		mbps = conf.BandwidthMbps
	}

	if err := s.pf.AddMapping(r.Context(), vmID, req.Protocol, req.HostPort, req.GuestPort, mbps, req.Description); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelPortFwd(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	proto := r.PathValue("protocol")
	hostPortStr := r.PathValue("hostPort")

	hostPort, err := strconv.Atoi(hostPortStr)
	if err != nil || hostPort <= 0 {
		http.Error(w, "invalid host_port", 400)
		return
	}
	if proto != "tcp" && proto != "udp" {
		http.Error(w, "protocol must be tcp or udp", 400)
		return
	}

	if err := s.pf.RemoveMapping(r.Context(), vmID, proto, hostPort); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSyncPortFwds(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var rules []portFwdEntry
	if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	mbps := 0
	if conf, _ := s.db.GetVMConfig(vmID); conf != nil {
		mbps = conf.BandwidthMbps
	}

	desired := make([]portforward.DesiredRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Protocol != "tcp" && rule.Protocol != "udp" {
			continue
		}
		desired = append(desired, portforward.DesiredRule{
			Protocol:    rule.Protocol,
			HostPort:    rule.HostPort,
			GuestPort:   rule.GuestPort,
			Description: rule.Description,
		})
	}

	if err := s.pf.SyncForVM(r.Context(), vmID, desired, mbps); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── TTY WebSocket ─────────────────────────────────────────────────────────────

// wsWriter 封装 WebSocket 连接为线程安全的 io.Writer，将容器输出写回客户端。
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// handleVMTTY 升级 HTTP 连接为 WebSocket，并将其与 VM 控制台双向绑定。
//
// 协议：
//   - 客户端 → 服务端：binary 帧为 stdin 数据；text 帧为 JSON 控制消息
//   - 控制消息：{"type":"resize","cols":80,"rows":24}
//   - 服务端 → 客户端：binary 帧为 stdout/stderr 原始字节流
func (s *Server) handleVMTTY(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stdinPR, stdinPW := io.Pipe()
	resizeCh := make(chan manager.ResizeEvent, 4)

	// 读取 WebSocket 消息：binary → 容器 stdin，text JSON → resize 事件
	go func() {
		defer stdinPW.Close()
		defer cancel()
		defer close(resizeCh)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				var ctrl struct {
					Type string `json:"type"`
					Cols uint   `json:"cols"`
					Rows uint   `json:"rows"`
				}
				if json.Unmarshal(msg, &ctrl) == nil && ctrl.Type == "resize" {
					select {
					case resizeCh <- manager.ResizeEvent{Cols: ctrl.Cols, Rows: ctrl.Rows}:
					default:
					}
				}
			} else {
				if _, err := stdinPW.Write(msg); err != nil {
					return
				}
			}
		}
	}()

	wc := &wsWriter{conn: conn}
	if err := s.mgr.AttachTTY(ctx, vmID, stdinPR, wc, resizeCh); err != nil && ctx.Err() == nil {
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("\r\n[disconnected: "+err.Error()+"]\r\n"))
	}
}
