package web

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cloudhv"
	"runman-agent/manager/portforward"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/disk"
	psnet "github.com/shirou/gopsutil/v4/net"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static/*
var staticFiles embed.FS

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type vmNetSnapshot struct {
	inBytes   int64
	outBytes  int64
	timestamp time.Time
}

type Server struct {
	db           *db.DB
	cfg          *config.Manager
	mgr          manager.VMManager
	cloudHVMgr   *cloudhv.Manager // 如果使用 CloudHV，保存直接引用以便内存上报
	hostMon      *monitor.HostMonitor
	pf           *portforward.Manager
	agent        interface{}
	vmSnapshots  map[string]*vmNetSnapshot
	vmSnapshotMu sync.Mutex
}

// NewServer 接收两个 manager：
// - mgr (svc) 是 VMService 包装层
// - rawMgr 是真正的驱动实现（CloudHV/Podman），用于内存上报等功能
func NewServer(database *db.DB, mgr manager.VMManager, hostMon *monitor.HostMonitor, cfg *config.Manager, pf *portforward.Manager, agent interface{}, rawMgr manager.VMManager) *Server {
	var cloudHVMgr *cloudhv.Manager
	if ch, ok := rawMgr.(*cloudhv.Manager); ok {
		cloudHVMgr = ch
	}

	return &Server{
		db:          database,
		cfg:         cfg,
		mgr:         mgr,
		cloudHVMgr:  cloudHVMgr,
		hostMon:     hostMon,
		pf:          pf,
		agent:       agent,
		vmSnapshots: make(map[string]*vmNetSnapshot),
	}
}

// authMiddleware enforces HTTP Basic Auth when WebUser is configured.
// If no credentials are stored in config the request passes through.
// /api/vm-status is always public (used by VMs to report status).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for VM status reporting endpoint (VMs use it)
		if r.URL.Path == "/api/vm-status" {
			next.ServeHTTP(w, r)
			return
		}

		conf := s.cfg.Get()
		if conf.WebUser == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != conf.WebUser ||
			bcrypt.CompareHashAndPassword([]byte(conf.WebPassHash), []byte(pass)) != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="narwhalcloud"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()

	// 系统
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/system/info", s.handleSystemInfo)
	mux.HandleFunc("/api/connection", s.handleConnection)

	// 镜像列表
	mux.HandleFunc("GET /api/images", s.handleImages)

	// VM 集合
	mux.HandleFunc("GET /api/vms", s.handleListVMs)
	mux.HandleFunc("POST /api/vms", s.handleCreateVM)

	// VM 单机操作
	mux.HandleFunc("GET /api/vms/{id}", s.handleGetVM)
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

	// 虚拟机状态上报
	mux.HandleFunc("POST /api/vm-status", s.handleVMStatus)

	// 静态文件
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := staticFiles.ReadFile("static/index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			if _, err := w.Write(data); err != nil {
				log.Printf("write response error: %v", err)
			}
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
		if _, err := w.Write(data); err != nil {
			log.Printf("write response error: %v", err)
		}
	})

	return http.ListenAndServe(addr, s.authMiddleware(mux))
}

// ─── 辅助 ──────────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
}

// ─── 系统 ──────────────────────────────────────────────────────────────────────

type hostStatusResponse struct {
	*monitor.HostStats
	MonthInTotal  int64 `json:"month_in_total"`
	MonthOutTotal int64 `json:"month_out_total"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conf := s.cfg.Get()
	ctx := context.WithValue(r.Context(), monitor.NICKey, conf.MonitorNIC)
	ctx = context.WithValue(ctx, monitor.DiskKey, conf.MonitorDisk)

	stats, err := s.hostMon.GetStats(ctx)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}

	// 汇总所有 VM 的月度流量
	vms, _ := s.mgr.ListVMs(ctx)
	var mIn, mOut int64
	for _, vm := range vms {
		if t, err := s.db.GetTraffic(vm.VmId); err == nil {
			mIn += t.MonthIn
			mOut += t.MonthOut
		}
	}

	jsonOK(w, hostStatusResponse{
		HostStats:     stats,
		MonthInTotal:  mIn,
		MonthOutTotal: mOut,
	})
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
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

type configRequest struct {
	Token          string `json:"token"`
	MonitorNIC     string `json:"monitor_nic"`
	MonitorDisk    string `json:"monitor_disk"`
	WebUser        string `json:"web_user"`
	WebPass        string `json:"web_pass"` // plaintext，服务端 bcrypt 后存储
	Host           string `json:"host"`
	MaxPortForward int32  `json:"max_port_forward"`
}

type configResponse struct {
	Token          string `json:"token"`
	MonitorNIC     string `json:"monitor_nic"`
	MonitorDisk    string `json:"monitor_disk"`
	WebUser        string `json:"web_user"`
	VirtType       string `json:"virt_type"`
	Host           string `json:"host"`
	MaxPortForward int32  `json:"max_port_forward"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req configRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// 原子更新配置文件
		err := s.cfg.Update(func(cfg *config.Config) {
			// 如果设置 Token，必须确保已有或正在设置用户名和密码
			if req.Token != "" {
				user := cfg.WebUser
				if req.WebUser != "" {
					user = req.WebUser
				}
				hasPass := cfg.WebPassHash != ""
				if req.WebPass != "" {
					hasPass = true
				}

				if user == "" || !hasPass {
					// 验证失败，稍后返回错误；这里无法返回，先保留状态
					return
				}
			}

			if req.Token != "" {
				cfg.Token = req.Token
			}
			if req.MonitorNIC != "" {
				cfg.MonitorNIC = req.MonitorNIC
			}
			if req.MonitorDisk != "" {
				cfg.MonitorDisk = req.MonitorDisk
			}
			if req.Host != "" {
				cfg.Host = req.Host
			}
			if req.MaxPortForward > 0 {
				cfg.MaxPortForward = req.MaxPortForward
			}
			if req.WebUser != "" {
				cfg.WebUser = req.WebUser
			}
			if req.WebPass != "" {
				hash, _ := bcrypt.GenerateFromPassword([]byte(req.WebPass), bcrypt.DefaultCost)
				cfg.WebPassHash = string(hash)
			}
		})
		if err != nil {
			http.Error(w, "failed to save config: "+err.Error(), 500)
			return
		}
		w.WriteHeader(200)
		return
	}
	conf := s.cfg.Get()
	jsonOK(w, configResponse{
		Token:          conf.Token,
		MonitorNIC:     conf.MonitorNIC,
		MonitorDisk:    conf.MonitorDisk,
		WebUser:        conf.WebUser,
		VirtType:       conf.VirtType,
		Host:           conf.Host,
		MaxPortForward: conf.MaxPortForward,
	})
}

func (s *Server) handleConnection(w http.ResponseWriter, _ *http.Request) {
	var connected bool
	var errMsg string
	if s.agent != nil {
		if a, ok := s.agent.(interface{ GetConnStatus() (bool, string) }); ok {
			connected, errMsg = a.GetConnStatus()
		}
	}
	jsonOK(w, map[string]interface{}{
		"connected": connected,
		"error":     errMsg,
	})
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

type vmListItem struct {
	VmId              string   `json:"vm_id"`
	Status            int32    `json:"status"`
	CpuPct            float32  `json:"cpu_pct"`
	RamUsedMb         int64    `json:"ram_used_mb"`
	RamTotalMb        int64    `json:"ram_total_mb"`
	NetInBps          int64    `json:"net_in_bps"`
	NetOutBps         int64    `json:"net_out_bps"`
	MonthlyTrafficIn  int64    `json:"monthly_traffic_in"`
	MonthlyTrafficOut int64    `json:"monthly_traffic_out"`
	Ips               []string `json:"ips"`
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.mgr.ListVMs(r.Context())

	s.vmSnapshotMu.Lock()
	defer s.vmSnapshotMu.Unlock()
	now := time.Now()

	items := make([]vmListItem, len(vms))
	for i, vm := range vms {
		// 从数据库获取流量数据（由 TrafficService 定期更新）
		traffic, _ := s.db.GetTraffic(vm.VmId)

		item := vmListItem{
			VmId:      vm.VmId,
			Status:    int32(vm.Status),
			CpuPct:    vm.CpuPct,
			RamUsedMb: vm.RamUsedMb,
			Ips:       vm.Ips,
		}

		// 从 DB 读取累计流量和月度流量
		if traffic != nil {
			item.MonthlyTrafficIn = traffic.MonthIn
			item.MonthlyTrafficOut = traffic.MonthOut
		}

		// 计算实时速率：当前网络计数器 - 上次缓存值，除以时间差
		// VM 的 TrafficInBytes/TrafficOutBytes 是驱动返回的累计计数器值
		if lastSnapshot, exists := s.vmSnapshots[vm.VmId]; exists {
			elapsed := now.Sub(lastSnapshot.timestamp).Seconds()
			if elapsed > 0.1 { // 至少间隔 100ms 才计算
				deltaIn := vm.TrafficInBytes - lastSnapshot.inBytes
				deltaOut := vm.TrafficOutBytes - lastSnapshot.outBytes
				// 防止计数器倒序（如容器重启）
				if deltaIn < 0 {
					deltaIn = 0
				}
				if deltaOut < 0 {
					deltaOut = 0
				}
				item.NetInBps = int64(float64(deltaIn) / elapsed)
				item.NetOutBps = int64(float64(deltaOut) / elapsed)
			}
		}

		// 保存当前快照用于下次计算速率
		s.vmSnapshots[vm.VmId] = &vmNetSnapshot{
			inBytes:   vm.TrafficInBytes,
			outBytes:  vm.TrafficOutBytes,
			timestamp: now,
		}

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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"vm_id": req.VmId})
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
	vmID := r.PathValue("id")
	if err := s.mgr.RestartVM(r.Context(), vmID); err != nil {
		jsonErr(w, err, 500)
		return
	}
	s.pf.RefreshVM(r.Context(), vmID)
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

	// 重新加载端口转发规则（因为重装可能分配了新的内网 IP）
	s.pf.RefreshVM(r.Context(), vmID)

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
	TargetAddr  string `json:"target_addr,omitempty"`
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
				TargetAddr:  e.TargetAddr,
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

	if err := s.pf.AddMapping(r.Context(), vmID, req.Protocol, req.HostPort, req.GuestPort, req.Description); err != nil {
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

	if err := s.pf.SyncForVM(r.Context(), vmID, desired); err != nil {
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

// handleVMStatus 接收虚拟机上报的状态信息：CPU占用率、内存使用情况（CloudHV only）
func (s *Server) handleVMStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		VmID       string  `json:"vm_id"`
		CpuPercent float32 `json:"cpu_percent"`
		RamUsedMb  int64   `json:"ram_used_mb"`
		RamTotalMb int64   `json:"ram_total_mb"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// 将状态信息交给 CloudHV Manager 处理（仅 CloudHV 支持）
	if s.cloudHVMgr != nil {
		s.cloudHVMgr.UpdateVMStatus(req.VmID, req.CpuPercent, req.RamUsedMb, req.RamTotalMb)
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	http.Error(w, "Not supported", http.StatusNotImplemented)
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
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stdinPR, stdinPW := io.Pipe()
	resizeCh := make(chan manager.ResizeEvent, 4)

	// 读取 WebSocket 消息：binary → 容器 stdin，text JSON → resize 事件
	go func() {
		defer func() { _ = stdinPW.Close() }()
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
