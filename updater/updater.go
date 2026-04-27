package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runman-agent/db"
	"strings"
	"time"
)

const (
	githubReleaseURL = "https://api.github.com/repos/narwhal-cloud/runman-agent/releases/latest"
	installScript    = "/opt/narwhal-agent/install.sh"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
}

type Service struct {
	database    *db.DB
	currentVer  string
	lastChecked time.Time
}

func NewService(database *db.DB, currentVer string) *Service {
	return &Service{
		database:   database,
		currentVer: currentVer,
	}
}

func (s *Service) Start(ctx context.Context) {
	log.Printf("[Updater] Service started (current: %s)", s.currentVer)

	// 如果是开发版本，不执行自动更新
	if s.currentVer == "dev" || s.currentVer == "" {
		log.Printf("[Updater] Dev version detected, auto-update disabled")
		return
	}

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	// 启动时先检查一次
	s.checkAndRun(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAndRun(ctx)
		}
	}
}

// ManualUpdate 供 Web 端或其他地方手动触发立即更新
func (s *Service) ManualUpdate() error {
	log.Printf("[Updater] Manual update requested")
	return s.executeUpdate()
}

func (s *Service) checkAndRun(ctx context.Context) {
	latest, err := s.fetchLatestVersion()
	if err != nil {
		log.Printf("[Updater] Fetch latest version failed: %v", err)
		return
	}

	// 如果版本一致，清理数据库中的待更新记录
	if latest == s.currentVer {
		// log.Printf("[Updater] Current version %s is up-to-date", s.currentVer)
		_ = s.database.SetSystem("pending_update_ver", "")
		return
	}

	log.Printf("[Updater] New version detected: %s (current: %s)", latest, s.currentVer)

	// 检查数据库中是否已有该版本的待更新记录
	pendingVer, _ := s.database.GetSystem("pending_update_ver")
	if pendingVer != latest {
		// 记录新版本发现时间，并随机生成一个 24-72 小时后的执行时间
		log.Printf("[Updater] Recording new update path for %s", latest)
		_ = s.database.SetSystem("pending_update_ver", latest)

		// 随机 24-72 小时 (86400 - 259200 秒)
		randomDelay := 86400 + rand.Intn(172800)
		targetTime := time.Now().Add(time.Duration(randomDelay) * time.Second)
		_ = s.database.SetSystem("update_target_time", targetTime.Format(time.RFC3339))

		log.Printf("[Updater] Scheduled update to %s at %s", latest, targetTime.Format("2006-01-02 15:04:05"))
		return
	}

	// 检查是否到达执行时间
	targetTimeStr, _ := s.database.GetSystem("update_target_time")
	if targetTimeStr == "" {
		return
	}

	targetTime, err := time.Parse(time.RFC3339, targetTimeStr)
	if err != nil {
		return
	}

	if time.Now().After(targetTime) {
		log.Printf("[Updater] REACHED target time! Executing forced update to %s...", latest)
		s.executeUpdate()
	} else {
		log.Printf("[Updater] Waiting for scheduled update at %s (remains: %v)",
			targetTime.Format("2006-01-02 15:04:05"),
			time.Until(targetTime).Round(time.Minute))
	}
}

func (s *Service) fetchLatestVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", githubReleaseURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api returned %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}

	return strings.TrimSpace(rel.TagName), nil
}

func (s *Service) executeUpdate() error {
	// 1. 先下载最新的安装脚本
	if err := s.downloadInstallScript(); err != nil {
		log.Printf("[Updater] Failed to download latest install script: %v", err)
		// 如果下载失败，尝试使用现有的（如果存在）
		if _, err := os.Stat(installScript); os.IsNotExist(err) {
			return err
		}
	}

	log.Printf("[Updater] Running %s...", installScript)

	// 使用 bash 运行安装脚本，传入 en 参数支持非交互模式
	cmd := exec.Command("bash", installScript, "en")
	cmd.Env = os.Environ()

	// 我们不等待脚本完成，因为脚本会重启服务导致我们被 kill
	err := cmd.Start()
	if err != nil {
		log.Printf("[Updater] Failed to start update script: %v", err)
		return err
	}

	log.Printf("[Updater] Update script started (PID: %d). Agent will be restarted by systemd.", cmd.Process.Pid)

	// 给一点时间让脚本运行并可能下载新版本
	go func() {
		time.Sleep(10 * time.Second)
		log.Printf("[Updater] Agent exiting for update...")
		os.Exit(0)
	}()

	return nil
}

func (s *Service) downloadInstallScript() error {
	const scriptURL = "https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", scriptURL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download script failed: status %d", resp.StatusCode)
	}

	// 确保目录存在
	_ = os.MkdirAll(filepath.Dir(installScript), 0755)

	f, err := os.Create(installScript)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
