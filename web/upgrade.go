package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UpgradeManager 管理升级过程
type UpgradeManager struct {
	currentBinary string
	currentVer    string
	backupDir     string
}

// NewUpgradeManager 创建升级管理器
func NewUpgradeManager(binaryPath, version string) *UpgradeManager {
	return &UpgradeManager{
		currentBinary: binaryPath,
		currentVer:    version,
		backupDir:     filepath.Join(filepath.Dir(binaryPath), ".backup"),
	}
}

// DownloadBinary 从 URL 下载二进制文件
func (um *UpgradeManager) DownloadBinary(ctx context.Context, downloadURL string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	// 临时文件
	tmpFile := filepath.Join(filepath.Dir(um.currentBinary), ".runman-agent.tmp")
	f, err := os.Create(tmpFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = os.Remove(tmpFile)
		return "", err
	}

	return tmpFile, nil
}

// VerifyBinary 验证下载的二进制文件
func (um *UpgradeManager) VerifyBinary(binaryPath string) error {
	// 检查文件是否存在且可执行
	stat, err := os.Stat(binaryPath)
	if err != nil {
		return err
	}

	if stat.Size() == 0 {
		return fmt.Errorf("binary file is empty")
	}

	// 尝试执行二进制文件的 --version 来验证它是一个有效的 runman-agent
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to verify binary: %v", err)
	}

	// 简单检查：输出应该包含 "narwhal" 或版本信息
	outputStr := string(output)
	if !strings.Contains(outputStr, "narwhal") && !strings.Contains(outputStr, "cloud") {
		return fmt.Errorf("binary verification failed: unexpected output")
	}

	return nil
}

// Backup 备份当前二进制
func (um *UpgradeManager) Backup() (string, error) {
	if err := os.MkdirAll(um.backupDir, 0755); err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	backupPath := filepath.Join(um.backupDir, fmt.Sprintf("runman-agent-%s-%s", um.currentVer, timestamp))

	if err := copyFile(um.currentBinary, backupPath); err != nil {
		return "", err
	}

	return backupPath, nil
}

// Apply 应用升级：用新二进制替换旧的
func (um *UpgradeManager) Apply(newBinaryPath string) error {
	// 先备份当前的二进制
	_, err := um.Backup()
	if err != nil {
		return fmt.Errorf("backup failed: %v", err)
	}

	// 替换二进制
	if err = os.Rename(newBinaryPath, um.currentBinary); err != nil {
		return err
	}

	// 确保二进制有执行权限
	if err = os.Chmod(um.currentBinary, 0755); err != nil {
		return err
	}

	return nil
}

// copyFile 复制文件
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	// 复制权限
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}

// UpgradeRfw 如果 rfw 二进制存在，从同一 GitHub release 下载对应架构的新版本并替换。
// 失败时只记录日志，不影响 agent 升级结果。
func (um *UpgradeManager) UpgradeRfw(ctx context.Context) {
	rfwPath := rfwBinaryPath()
	if _, err := os.Stat(rfwPath); err != nil {
		return // rfw 未安装，跳过
	}

	arch := "amd64"
	if isARM64() {
		arch = "arm64"
	}

	_, downloadURL, err := fetchRfwLatestRelease(arch)
	if err != nil || downloadURL == "" {
		log.Printf("[rfw] update check failed: %v", err)
		return
	}

	tmpPath := rfwPath + ".tmp"
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		log.Printf("[rfw] build request: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[rfw] download failed: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[rfw] download failed: HTTP %d", resp.StatusCode)
		return
	}

	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("[rfw] create temp file: %v", err)
		return
	}
	if _, err = io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		log.Printf("[rfw] write failed: %v", err)
		return
	}
	_ = f.Close()

	if err = os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("[rfw] chmod: %v", err)
		return
	}
	if err = os.Rename(tmpPath, rfwPath); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("[rfw] rename: %v", err)
		return
	}
	log.Printf("[rfw] updated to latest release")
}

// fetchRfwLatestRelease 从 GitHub releases（与 agent 同仓库）查找 rfw-amd64 / rfw-arm64 资产。
func fetchRfwLatestRelease(arch string) (version, downloadURL string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/narwhal-cloud/runman-agent/releases/latest")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github api returned %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}
	for _, asset := range release.Assets {
		if asset.Name == "rfw-"+arch {
			return release.TagName, asset.DownloadURL, nil
		}
	}
	return release.TagName, "", nil
}

// CleanupBackups 清理过期的备份（保留最近5个）
func (um *UpgradeManager) CleanupBackups() error {
	entries, err := os.ReadDir(um.backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if len(entries) <= 5 {
		return nil
	}

	// 删除最旧的备份
	for i := 0; i < len(entries)-5; i++ {
		oldBackup := filepath.Join(um.backupDir, entries[i].Name())
		_ = os.Remove(oldBackup)
	}

	return nil
}
