# Narwhal Cloud Agent Gateway 协议对接指南

[English](PROTOCOL.en.md)

本文档面向希望自行实现 Agent 端的开发者，说明如何通过 `AgentGateway` gRPC 协议接入 Narwhal Cloud 平台。

---

## 目录

1. [概述](#1-概述)
2. [连接建立与认证](#2-连接建立与认证)
3. [心跳上报](#3-心跳上报)
4. [命令执行流程](#4-命令执行流程)
5. [VM 生命周期命令](#5-vm-生命周期命令)
6. [端口转发命令](#6-端口转发命令)
7. [控制台 TTY](#7-控制台-tty)
8. [保活机制](#8-保活机制)
9. [断线重连与命令重放](#9-断线重连与命令重放)
10. [代码生成](#10-代码生成)
11. [最小实现示例（Go）](#11-最小实现示例go)

---

## 1. 概述

```
┌─────────────────────────┐   gRPC bidirectional stream   ┌──────────────────────────┐
│  Agent（你的实现）         │ ══════════════════════════════ │  Narwhal Cloud Platform  │
│  运行于宿主机              │   AgentEnvelope  →            │  gRPC Server             │
│                         │   ← PlatformEnvelope          │                          │
└─────────────────────────┘                               └──────────────────────────┘
```

- **Agent 主动连接**平台 gRPC 端点，平台作为服务端。
- 连接建立后，双方通过一条**持久的双向流**通信，无需为每条消息重新握手。
- Agent 负责管理宿主机上所有 VM 的完整生命周期（创建、启停、删除、重装等）。
- 平台不主动连接 Agent，所有控制命令通过流下发。

**协议文件**：[proto/agent/agent.proto](proto/agent/agent.proto)

---

## 2. 连接建立与认证

### 2.1 gRPC 端点

向平台申请 `agent_token` 和 gRPC 地址后，Agent 通过标准 gRPC 拨号连接平台。建议启用 TLS。

### 2.2 身份认证

在每次调用 `Connect` RPC 时，通过 **gRPC metadata** 携带令牌：

```
key:   authorization
value: Bearer <agent_token>
```

Go 示例：

```go
import "google.golang.org/grpc/metadata"

ctx := metadata.AppendToOutgoingContext(context.Background(),
    "authorization", "Bearer "+agentToken,
)
stream, err := client.Connect(ctx)
```

### 2.3 建立流

```go
stream, err := agentClient.Connect(ctx)
// stream 是 grpc.BidiStreamingClient[AgentEnvelope, PlatformEnvelope]
// 上行：stream.Send(&AgentEnvelope{...})
// 下行：stream.Recv() 返回 *PlatformEnvelope
```

连接成功后立即发送第一条 **Heartbeat**，随后每 **30 秒**发送一次。

---

## 3. 心跳上报

心跳是 Agent 向平台上报宿主机状态的主要手段，平台依赖心跳进行对账和异常检测。

### 3.1 上报频率

每 **30 秒**发送一次，不得超过 60 秒。

### 3.2 Heartbeat 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `timestamp` | int64 | ✓ | Unix 秒时间戳 |
| `cpu_pct` | float | ✓ | 宿主机 CPU 使用率，0–100 |
| `ram_used_mb` | int64 | ✓ | 已用内存（MB） |
| `disk_used_gb` | int64 | ✓ | 已用磁盘（GB） |
| `net_in_bps` | int64 | ✓ | 实时入口流速（bytes/s） |
| `net_out_bps` | int64 | ✓ | 实时出口流速（bytes/s） |
| `load1/5/15` | float | ✓ | 系统负载均值 |
| `uptime` | int64 | ✓ | 系统运行时间（秒） |
| `cpus` | int32 | ✓ | 逻辑 CPU 核数 |
| `ram_total_mb` | int64 | ✓ | 总内存（MB） |
| `disk_total_gb` | int64 | ✓ | 总磁盘（GB） |
| `bandwidth_mbps` | int32 | ✓ | 总带宽（Mbps） |
| `virt_type` | string | ✓ | 虚拟化后端：`podman` / `cloudhv` |
| `vms` | VMSummary[] | ✓ | 当前所有 VM 的状态摘要 |
| `entry_host` | string | 变化时 | 宿主机 IPv4 / DDNS 域名 |
| `entry_ipv6` | string | 变化时 | 宿主机 IPv6 地址（可选） |
| `os_images` | OSImageInfo[] | 变化时 | 支持的 OS 镜像列表 |

> `entry_host`、`entry_ipv6`、`os_images` 只在**值发生变化时**填写，未变化可留空，平台会保留上次的值。

### 3.3 VMSummary 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `vm_id` | string | VM UUID（由平台分配） |
| `status` | VMStatus | 当前运行状态 |
| `cpu_pct` | float | VM CPU 使用率，0–100 |
| `ram_used_mb` | int64 | VM 已用内存（MB） |
| `traffic_in_bytes` | int64 | 累计入站流量（bytes，自创建起） |
| `traffic_out_bytes` | int64 | 累计出站流量（bytes，自创建起） |
| `ips` | string[] | VM IP 地址列表 |
| `monthly_traffic_in` | int64 | 本月累计入站流量（bytes） |
| `monthly_traffic_out` | int64 | 本月累计出站流量（bytes） |

### 3.4 VMStatus 枚举

| 值 | 含义 |
|----|------|
| `VM_STATUS_CREATING` | 创建中（平台等待 Agent 回报） |
| `VM_STATUS_RUNNING` | 运行中 |
| `VM_STATUS_STOPPED` | 已停止 |
| `VM_STATUS_ERROR` | 异常 |

---

## 4. 命令执行流程

平台下发的每条命令遵循**请求-回执**模式：

```
Platform                          Agent
   │                                │
   │── PlatformEnvelope ──────────►│  下发命令，携带唯一 command_id
   │   command_id = "cmd-uuid-123" │
   │   payload = CmdStartVM{...}   │
   │                                │
   │                           执行操作
   │                                │
   │◄─ AgentEnvelope ───────────────│  回报结果
   │   payload = CommandResult{     │
   │     command_id = "cmd-uuid-123"│
   │     success = true             │
   │   }                            │
```

**关键规则：**

1. 每条 `PlatformEnvelope` 命令**必须**回报一条 `CommandResult`，包括失败情况。
2. `CommandResult.command_id` 须与对应命令的 `command_id` 完全一致。
3. **例外：`Ping` 消息无需回报 `CommandResult`。**
4. 命令可并发执行；回报顺序不要求与接收顺序一致。

### 4.1 CommandResult 结构

```protobuf
message CommandResult {
  string command_id = 1; // 对应命令的 command_id
  bool   success    = 2; // 是否成功
  string error      = 3; // 失败原因（success=false 时填写）
  bytes  data       = 4; // JSON 编码的附加数据（特定命令使用）
}
```

### 4.2 AgentEnvelope 上行消息类型

| oneof 字段 | 触发时机 |
|------------|----------|
| `heartbeat` | 每 30 秒定期上报 |
| `cmd_result` | 执行任意命令后回报 |
| `port_fwd_list` | 响应 `CmdGetPortForwards` |
| `console_output` | 控制台 TTY 有输出时 |
| `console_event` | 控制台会话状态变化时 |

---

## 5. VM 生命周期命令

### 5.1 CmdCreateVM — 创建 VM

平台下发此命令时，Agent 应：

1. 按照指定规格在宿主机上创建并启动 VM。
2. 成功后在下一次心跳中上报 `VMStatus = VM_STATUS_RUNNING`。
3. 通过 `CommandResult{success: true}` 回报。

| 字段 | 类型 | 说明 |
|------|------|------|
| `vm_id` | string | VM UUID，Agent 以此作为本地唯一标识符 |
| `cpu` | int32 | vCPU 核数 |
| `ram_mb` | int64 | 内存（MB） |
| `disk_gb` | int64 | 磁盘（GB） |
| `bandwidth_mbps` | int32 | 带宽限速（Mbps），0 = 不限 |
| `os_image` | string | OS 镜像 ID，须与心跳中上报的 `OSImageInfo.id` 一致 |
| `root_password` | string | root 初始密码（明文） |

### 5.2 CmdStartVM — 启动 VM

启动一台已停止的 VM。成功后下一次心跳上报 `RUNNING`。

### 5.3 CmdStopVM — 关闭 VM

| 字段 | 说明 |
|------|------|
| `vm_id` | VM UUID |
| `force` | `true` = 强制断电（硬关机），`false` = 优雅关机（ACPI） |

### 5.4 CmdRestartVM — 重启 VM

执行优雅重启。如需强制重启，平台会连续下发 `CmdStopVM{force:true}` + `CmdStartVM`。

### 5.5 CmdDeleteVM — 销毁 VM

**不可恢复操作**，释放所有关联资源（磁盘镜像、IP、端口转发规则等）。  
平台在下发此命令前已完成计费清算。

### 5.6 CmdReinstallVM — 重装系统

原磁盘数据将被清除。建议执行顺序：停止 VM → 销毁旧实例 → 用新镜像重新创建 → 启动。

| 字段 | 说明 |
|------|------|
| `os_image` | 新 OS 镜像 ID |
| `root_password` | 新 root 密码 |
| `cpu/ram_mb/disk_gb/bandwidth_mbps` | 可能随套餐变更的规格参数 |

### 5.7 CmdResetPassword — 重置 root 密码

Agent 需将新密码写入 VM 内部（如通过 cloud-init、`chroot` + `chpasswd` 等方式）。  
密码为明文传输，建议平台侧在传输层启用 TLS。

---

## 6. 端口转发命令

Agent 需在宿主机的网络层（如 `iptables`/`nftables`）维护端口转发规则，将宿主机端口流量转发至对应 VM 的内部端口。

### 6.1 CmdSetPortForward — 新增规则

| 字段 | 说明 |
|------|------|
| `vm_id` | VM UUID |
| `protocol` | `PROTOCOL_TCP` 或 `PROTOCOL_UDP` |
| `host_port` | 宿主机（外部）端口，建议范围 1024–65535 |
| `guest_port` | VM 内部端口 |
| `description` | 备注（如 "SSH"、"HTTP"） |

### 6.2 CmdDelPortForward — 删除规则

通过 `{vm_id, protocol, host_port}` 三元组唯一定位并删除规则。

### 6.3 CmdGetPortForwards — 查询规则列表

**注意**：此命令的响应不使用 `CommandResult.data`，而是通过 `AgentEnvelope.port_fwd_list` 字段单独上报：

```go
// 响应示例（Go）
stream.Send(&AgentEnvelope{
    MessageId: uuid.New().String(),
    Payload: &AgentEnvelope_PortFwdList{
        PortFwdList: &PortForwardList{
            CommandId: cmd.CommandId,  // 对应命令的 command_id
            Entries:   entries,
        },
    },
})
// 同时仍需发送 CommandResult 确认命令已处理
stream.Send(&AgentEnvelope{
    MessageId: uuid.New().String(),
    Payload: &AgentEnvelope_CmdResult{
        CmdResult: &CommandResult{
            CommandId: cmd.CommandId,
            Success:   true,
        },
    },
})
```

---

## 7. 控制台 TTY

控制台功能允许用户通过平台的 WebSocket 接口直接与 VM 的 TTY 交互。

### 7.1 会话建立流程

```
Platform                                    Agent
   │                                          │
   │── CmdConsoleOpen ─────────────────────►│
   │   vm_id = "vm-uuid"                    │
   │   session_id = "sess-uuid"              │  ← 本次会话的唯一标识
   │   cols = 220, rows = 50                │
   │                                          │
   │                              附接 VM TTY（如 podman exec -it）
   │                                          │
   │◄─ ConsoleEvent{CONNECTED} ──────────────│  附接成功
   │                                          │
   │◄─── CommandResult{success:true} ────────│  命令回执
   │                                          │
   │   [用户开始输入]                          │
   │── CmdConsoleInput ──────────────────────►│
   │   session_id = "sess-uuid"              │
   │   data = <stdin bytes>                  │
   │                                          │
   │◄─ ConsoleOutput ────────────────────────│
   │   session_id = "sess-uuid"              │
   │   data = <stdout bytes>                 │
   │                                          │
   │── CmdConsoleResize ─────────────────────►│
   │   cols = 240, rows = 60                 │  终端尺寸变化
   │                                          │
   │── CmdConsoleClose ──────────────────────►│
   │   session_id = "sess-uuid"              │
   │                                          │
   │◄─ ConsoleEvent{DISCONNECTED} ───────────│
   │◄─── CommandResult{success:true} ────────│
```

### 7.2 各消息职责

| 方向 | 消息 | 说明 |
|------|------|------|
| 下行 | `CmdConsoleOpen` | 请求 Agent 打开 TTY 会话 |
| 下行 | `CmdConsoleInput` | 将用户键盘输入（含控制字符）写入 VM stdin |
| 下行 | `CmdConsoleResize` | 调整终端尺寸（`ioctl TIOCSWINSZ`） |
| 下行 | `CmdConsoleClose` | 关闭会话，Agent 清理 TTY 资源 |
| 上行 | `ConsoleOutput` | VM TTY 输出的原始字节流 |
| 上行 | `ConsoleEvent` | 会话状态变化（CONNECTED / DISCONNECTED / ERROR） |

### 7.3 错误处理

- 若 VM 不存在或无法附接 TTY，上报 `ConsoleEvent{type: ERROR, reason: "..."}`，同时回报 `CommandResult{success: false, error: "..."}`。
- 若 TTY 在会话中途断开（VM 重启、崩溃等），主动上报 `ConsoleEvent{type: DISCONNECTED, reason: "..."}`，无需等待 `CmdConsoleClose`。

### 7.4 session_id 的作用

`session_id` 由平台生成，Agent 需用它关联所有控制台帧。单个宿主机上可能同时存在多个活跃的控制台会话（不同 VM 或同一 VM 的多个会话），Agent 须用 `session_id` 区分它们。

---

## 8. 保活机制

平台会定期下发 `Ping` 消息，防止代理层（如 Nginx、CDN）因长时间无数据而断开连接。

**Agent 收到 `Ping` 后无需任何响应，也无需发送 `CommandResult`。**

```protobuf
message Ping {}  // 空消息，忽略即可
```

Agent 侧也可以根据需要启用 gRPC KeepAlive：

```go
conn, err := grpc.Dial(addr,
    grpc.WithKeepaliveParams(keepalive.ClientParameters{
        Time:                30 * time.Second,
        Timeout:             10 * time.Second,
        PermitWithoutStream: true,
    }),
)
```

---

## 9. 断线重连与命令重放

### 9.1 重连策略

连接断开后，Agent 须立即尝试重连，建议使用指数退避：

```
第 1 次重连：等待 1s
第 2 次重连：等待 2s
第 3 次重连：等待 4s
...
上限：等待 60s
```

### 9.2 命令幂等性

平台**不会**在重连后重放未确认的命令。Agent 断线期间错过的命令将不会被补发，因此 Agent 应尽快完成重连并恢复心跳，以便平台感知到节点状态。

即便如此，各命令的实现仍建议保持幂等，以应对极少数情况下的重复下发：

- `CmdCreateVM`：VM 已存在则直接回报成功。
- `CmdDeleteVM`：VM 已不存在则直接回报成功。
- `CmdSetPortForward`：规则已存在则覆盖或跳过。
- `CmdDelPortForward`：规则不存在则视为成功。

### 9.3 消息去重

每条 `AgentEnvelope` 携带 `message_id`（建议 UUID v4）。平台侧可基于此字段对重复上报的消息进行去重（如网络抖动导致的重传）。

---

## 10. 代码生成

安装 `protoc` 及 Go 插件后，在项目根目录执行：

```bash
# 安装插件
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# 生成代码
protoc \
  --go_out=. \
  --go_opt=paths=source_relative \
  --go-grpc_out=. \
  --go-grpc_opt=paths=source_relative \
  proto/agent/agent.proto
```

其他语言（Python、Rust、TypeScript 等）同理，替换对应的 `protoc` 插件即可。

---

## 11. 最小实现示例（Go）

以下示例演示 Agent 的核心连接逻辑，省略了具体的 VM 管理实现。

```go
package main

import (
    "context"
    "log"
    "time"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/metadata"

    pb "your-module/proto/agent"
)

const (
    platformAddr = "grpc.example.com:50051"
    agentToken   = "your-agent-token"
)

func main() {
    conn, err := grpc.Dial(platformAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    client := pb.NewAgentGatewayClient(conn)

    for {
        if err := runStream(client); err != nil {
            log.Printf("stream error: %v, reconnecting in 5s...", err)
            time.Sleep(5 * time.Second)
        }
    }
}

func runStream(client pb.AgentGatewayClient) error {
    ctx := metadata.AppendToOutgoingContext(context.Background(),
        "authorization", "Bearer "+agentToken,
    )
    stream, err := client.Connect(ctx)
    if err != nil {
        return err
    }

    // 启动心跳 goroutine
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        // 立即发送第一次心跳
        sendHeartbeat(stream)
        for range ticker.C {
            if err := sendHeartbeat(stream); err != nil {
                return
            }
        }
    }()

    // 接收并处理平台命令
    for {
        envelope, err := stream.Recv()
        if err != nil {
            return err
        }
        go handleCommand(stream, envelope)
    }
}

func handleCommand(stream pb.AgentGateway_ConnectClient, env *pb.PlatformEnvelope) {
    switch p := env.Payload.(type) {
    case *pb.PlatformEnvelope_Ping:
        // Ping 无需回复

    case *pb.PlatformEnvelope_CreateVm:
        err := createVM(p.CreateVm)
        replyResult(stream, env.CommandId, err)

    case *pb.PlatformEnvelope_StartVm:
        err := startVM(p.StartVm.VmId)
        replyResult(stream, env.CommandId, err)

    case *pb.PlatformEnvelope_StopVm:
        err := stopVM(p.StopVm.VmId, p.StopVm.Force)
        replyResult(stream, env.CommandId, err)

    case *pb.PlatformEnvelope_DeleteVm:
        err := deleteVM(p.DeleteVm.VmId)
        replyResult(stream, env.CommandId, err)

    // ... 其余命令类型
    }
}

func replyResult(stream pb.AgentGateway_ConnectClient, cmdID string, err error) {
    result := &pb.CommandResult{CommandId: cmdID, Success: err == nil}
    if err != nil {
        result.Error = err.Error()
    }
    stream.Send(&pb.AgentEnvelope{
        MessageId: newUUID(),
        Payload:   &pb.AgentEnvelope_CmdResult{CmdResult: result},
    })
}

func sendHeartbeat(stream pb.AgentGateway_ConnectClient) error {
    hb := &pb.Heartbeat{
        Timestamp:   time.Now().Unix(),
        CpuPct:      getCPUPercent(),
        RamUsedMb:   getRAMUsed(),
        // ... 填充其他字段
        Vms: getVMSummaries(),
    }
    return stream.Send(&pb.AgentEnvelope{
        MessageId: newUUID(),
        Payload:   &pb.AgentEnvelope_Heartbeat{Heartbeat: hb},
    })
}
```

---

*协议版本：v1.0 — 如有更新，请以 [agent.proto](proto/agent/agent.proto) 文件头部的版本号为准。Narwhal Cloud © 2026*
