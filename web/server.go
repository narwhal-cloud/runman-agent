package web

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/monitor"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
	psnet "github.com/shirou/gopsutil/v4/net"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	db      *db.DB
	mgr     manager.VMManager
	hostMon *monitor.HostMonitor
}

func NewServer(database *db.DB, mgr manager.VMManager, hostMon *monitor.HostMonitor) *Server {
	return &Server{
		db:      database,
		mgr:     mgr,
		hostMon: hostMon,
	}
}

func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/vms", s.handleVMs)
	mux.HandleFunc("/api/system/info", s.handleSystemInfo)

	// Use FileServer to serve static files
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, _ := staticFiles.ReadFile("static/index.html")
			w.Header().Set("Content-Type", "text/html")
			w.Write(data)
			return
		}

		// Map URL path to static folder
		filePath := "static" + r.URL.Path
		data, err := staticFiles.ReadFile(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// Simple content type detection
		if strings.HasSuffix(filePath, ".png") {
			w.Header().Set("Content-Type", "image/png")
		}
		w.Write(data)
	})

	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conf, _ := s.db.GetConfig()
	ctx := context.WithValue(r.Context(), "monitor_nic", conf.MonitorNIC)
	ctx = context.WithValue(ctx, "monitor_disk", conf.MonitorDisk)

	stats, err := s.hostMon.GetStats(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
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

	json.NewEncoder(w).Encode(map[string][]string{
		"nics":  nicNames,
		"disks": mountPoints,
	})
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
			// 只允许修改这些字段
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
	json.NewEncoder(w).Encode(conf)
}

func (s *Server) handleVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.mgr.ListVMs(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vms)
}
