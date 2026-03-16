//go:build containers_image_openpgp

package podman

// 测试前置条件（参考 agent.sh 安装脚本）：
//   1. podman 已安装并 socket 可用：systemctl start podman.socket
//   2. fuckme 网络已创建：
//      podman network create --driver=bridge \
//        --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
//        --ipv6 --subnet=fd91:cafe:cafe:10::/64 --gateway=fd91:cafe:cafe:10::1 \
//        fuckme
//   3. lxcfs 已安装并运行：apt install lxcfs && systemctl start lxcfs
//   4. 镜像可拉取：docker.io/narwhalcloud/alpine:podman
//
// 运行命令：
//   go test -v -tags containers_image_openpgp -run TestPodman ./manager/podman/ -timeout 300s

import (
	"context"
	"os"
	"testing"

	"runman-agent/proto/agent"
)

const (
	testSocket   = "unix:///run/podman/podman.sock"
	testVmID     = "test-vm-001"
	testImage    = "docker.io/narwhalcloud/alpine:podman"
	testPassword = "Test1234!"
)

// newTestManager 创建 Manager，环境不满足时 Skip
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	if _, err := os.Stat("/run/podman/podman.sock"); err != nil {
		t.Skipf("podman socket 不存在，跳过测试: %v", err)
	}
	m, err := New(testSocket)
	if err != nil {
		t.Skipf("无法连接 podman socket，跳过测试: %v", err)
	}
	return m
}

// ensureClean 确保测试开始前没有残留容器
func ensureClean(m *Manager, vmID string) {
	_ = m.DeleteVM(context.Background(), vmID)
}

// TestPodmanCreateAndDelete 测试创建与删除
func TestPodmanCreateAndDelete(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:          testVmID,
		Cpu:           1,
		RamMb:         128,
		DiskGb:        1,
		BandwidthMbps: 100,
		OsImage:       testImage,
		RootPassword:  testPassword,
	})
	if err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	if err = m.DeleteVM(ctx, testVmID); err != nil {
		t.Fatalf("DeleteVM 失败: %v", err)
	}
}

// TestPodmanStartStopRestart 测试生命周期管理
func TestPodmanStartStopRestart(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	t.Run("Stop", func(t *testing.T) {
		if err := m.StopVM(ctx, testVmID, false); err != nil {
			t.Fatalf("StopVM 失败: %v", err)
		}
	})
	t.Run("Start", func(t *testing.T) {
		if err := m.StartVM(ctx, testVmID); err != nil {
			t.Fatalf("StartVM 失败: %v", err)
		}
	})
	t.Run("Restart", func(t *testing.T) {
		if err := m.RestartVM(ctx, testVmID); err != nil {
			t.Fatalf("RestartVM 失败: %v", err)
		}
	})
	t.Run("ForceStop", func(t *testing.T) {
		if err := m.StopVM(ctx, testVmID, true); err != nil {
			t.Fatalf("ForceStop 失败: %v", err)
		}
		_ = m.StartVM(ctx, testVmID)
	})
}

// TestPodmanGetVMInfo 测试状态与信息查询
func TestPodmanGetVMInfo(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	info, err := m.GetVMInfo(ctx, testVmID)
	if err != nil {
		t.Fatalf("GetVMInfo 失败: %v", err)
	}
	if info.VmId != testVmID {
		t.Errorf("VmId = %q，期望 %q", info.VmId, testVmID)
	}
	if info.Status != agent.VMStatus_VM_STATUS_RUNNING {
		t.Errorf("Status = %v，期望 RUNNING", info.Status)
	}
	if info.Ip == "" {
		t.Error("IP 不应为空")
	}
	t.Logf("VM 状态: ip=%s cpu=%.1f%% mem=%dMB netIn=%dB netOut=%dB",
		info.Ip, info.CpuPct, info.RamUsedMb, info.TrafficInBytes, info.TrafficOutBytes)
}

// TestPodmanGetVMIP 测试 IP 获取
func TestPodmanGetVMIP(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	ip, err := m.GetVMIP(ctx, testVmID)
	if err != nil {
		t.Fatalf("GetVMIP 失败: %v", err)
	}
	if ip == "" {
		t.Error("IP 不应为空")
	}
	t.Logf("容器 IP: %s", ip)
}

// TestPodmanUpdateVM 测试资源配置更新
func TestPodmanUpdateVM(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	if err := m.UpdateVM(ctx, &agent.CmdUpdateVM{
		VmId:  testVmID,
		Cpu:   2,
		RamMb: 256,
	}); err != nil {
		t.Fatalf("UpdateVM 失败: %v", err)
	}
}

// TestPodmanResetPassword 测试密码重置
func TestPodmanResetPassword(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	if err := m.ResetPassword(ctx, testVmID, "NewPass5678!"); err != nil {
		t.Fatalf("ResetPassword 失败: %v", err)
	}
}

// TestPodmanReinstallVM 测试重装系统
func TestPodmanReinstallVM(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	if err := m.ReinstallVM(ctx, &agent.CmdReinstallVM{
		VmId:         testVmID,
		OsImage:      testImage,
		RootPassword: "Reinstalled99!",
	}); err != nil {
		t.Fatalf("ReinstallVM 失败: %v", err)
	}

	info, err := m.GetVMInfo(ctx, testVmID)
	if err != nil {
		t.Fatalf("重装后 GetVMInfo 失败: %v", err)
	}
	if info.Status != agent.VMStatus_VM_STATUS_RUNNING {
		t.Errorf("重装后状态 = %v，期望 RUNNING", info.Status)
	}
}

// TestPodmanListVMs 测试容器列表
func TestPodmanListVMs(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	ensureClean(m, testVmID)

	if err := m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         testVmID,
		Cpu:          1,
		RamMb:        128,
		DiskGb:       1,
		OsImage:      testImage,
		RootPassword: testPassword,
	}); err != nil {
		t.Fatalf("CreateVM 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteVM(ctx, testVmID) })

	list, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs 失败: %v", err)
	}

	found := false
	for _, v := range list {
		if v.VmId == testVmID {
			found = true
		}
		t.Logf("  VM: id=%s status=%v ip=%s", v.VmId, v.Status, v.Ip)
	}
	if !found {
		t.Errorf("ListVMs 中未找到 %q", testVmID)
	}
}

// TestPodmanGetSupportedImages 测试镜像列表（无需运行中的容器）
func TestPodmanGetSupportedImages(t *testing.T) {
	m := newTestManager(t)
	imgs, err := m.GetSupportedImages(context.Background())
	if err != nil {
		t.Fatalf("GetSupportedImages 失败: %v", err)
	}
	if len(imgs) == 0 {
		t.Error("至少应返回一个镜像")
	}
	for _, img := range imgs {
		t.Logf("  镜像: id=%s name=%s", img.Id, img.Name)
	}
}
