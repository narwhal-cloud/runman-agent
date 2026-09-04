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
	"regexp"
	"runman-agent/db"
	"strings"
	"sync"
	"time"
)

const (
	releaseRepository = "podcctv/runman-agent"
	installScript     = "/opt/narwhal-agent/install.sh"
)

type githubRelease struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
}

type Service struct {
	database    *db.DB
	currentVer  string
	lastChecked time.Time
	updateMu    sync.Mutex
}

// Rolling releases must compare the embedded commit, not the constant tag
// "continuous" (or the old build version "main"). Tagged builds stay stable.
func releaseChannel(version string) string {
	if strings.HasPrefix(version, "v") {
		return "latest"
	}
	return "continuous"
}

func releaseAPIURL(version string) string {
	base := "https://api.github.com/repos/" + releaseRepository + "/releases/"
	if releaseChannel(version) == "latest" {
		return base + "latest"
	}
	return base + "tags/continuous"
}

func releaseDownloadBase(version string) string {
	base := "https://github.com/" + releaseRepository + "/releases/"
	if releaseChannel(version) == "latest" {
		return base + "latest/download"
	}
	return base + "download/continuous"
}

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func releaseVersion(rel githubRelease, channel string) (string, error) {
	if channel == "continuous" {
		sha := strings.TrimSpace(rel.TargetCommitish)
		if rel.TagName != "continuous" || !commitPattern.MatchString(sha) {
			return "", fmt.Errorf("continuous release has no immutable commit identity")
		}
		return "continuous-" + strings.ToLower(sha), nil
	}
	if !strings.HasPrefix(rel.TagName, "v") {
		return "", fmt.Errorf("invalid stable release tag %q", rel.TagName)
	}
	return rel.TagName, nil
}

func NewService(database *db.DB, currentVer string) *Service {
	return &Service{
		database:   database,
		currentVer: currentVer,
	}
}

func (s *Service) Start(ctx context.Context) {
	log.Printf("[Updater] Service started (current: %s)", s.currentVer)
	if os.Getenv("RUNMAN_AGENT_AUTO_UPDATE") == "0" {
		log.Printf("[Updater] Automatic updates disabled by RUNMAN_AGENT_AUTO_UPDATE=0")
		return
	}

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
		_ = s.database.SetSystem("update_target_time", "")
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
		if err := s.executeUpdate(); err != nil {
			log.Printf("[Updater] Update failed; Agent remains running: %v", err)
		}
	} else {
		log.Printf("[Updater] Waiting for scheduled update at %s (remains: %v)",
			targetTime.Format("2006-01-02 15:04:05"),
			time.Until(targetTime).Round(time.Minute))
	}
}

func (s *Service) fetchLatestVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", releaseAPIURL(s.currentVer), nil)
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

	return releaseVersion(rel, releaseChannel(s.currentVer))
}

func (s *Service) executeUpdate() error {
	if !s.updateMu.TryLock() {
		return fmt.Errorf("update already being prepared")
	}
	defer s.updateMu.Unlock()
	// 1. 先下载最新的安装脚本
	if err := s.downloadInstallScript(); err != nil {
		log.Printf("[Updater] Failed to download latest install script: %v", err)
		// Never fall back to a cached script: it may belong to upstream.
		return err
	}
	if output, err := exec.Command("bash", "-n", installScript).CombinedOutput(); err != nil {
		return fmt.Errorf("installer syntax check: %w: %s", err, output)
	}

	log.Printf("[Updater] Running %s...", installScript)

	// A separate systemd unit survives stopping/restarting the Agent, including
	// upstream installations with KillMode=control-group. A fixed unit name also
	// rejects concurrent manual/automatic update jobs.
	cmd := exec.Command("systemd-run", updateCommandArgs(s.currentVer)...)

	// 我们不等待脚本完成，因为脚本会重启服务导致我们被 kill
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[Updater] Failed to start update script: %v", err)
		return fmt.Errorf("start update unit: %w: %s", err, output)
	}

	log.Printf("[Updater] Update unit started; inspect journalctl -u narwhal-agent-update")

	return nil
}

func updateCommandArgs(version string) []string {
	return []string{"--unit=narwhal-agent-update", "--collect", "--property=Type=exec",
		"--property=WorkingDirectory=/", "--setenv=AGENT_RELEASE_TAG=" + releaseChannel(version),
		"--setenv=RUNMAN_AGENT_DOWNLOAD_BASE=" + releaseDownloadBase(version),
		"bash", installScript, "en", "--update-only", "--non-interactive"}
}

func (s *Service) downloadInstallScript() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return downloadScript(ctx, http.DefaultClient, releaseDownloadBase(s.currentVer)+"/install.sh", installScript)
}

func downloadScript(ctx context.Context, client *http.Client, scriptURL, destination string) error {

	req, err := http.NewRequestWithContext(ctx, "GET", scriptURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download script failed: status %d", resp.StatusCode)
	}

	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(destination), ".installer-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	const maxScriptSize = 2 << 20
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxScriptSize+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxScriptSize {
		return fmt.Errorf("invalid installer size: %d", n)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), destination)
}
