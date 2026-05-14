# Narwhal Cloud Agent Gateway — Integration Guide

[中文](PROTOCOL.md)

This document is for developers who want to implement their own Agent and integrate with the Narwhal Cloud platform via the `AgentGateway` gRPC protocol.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Connection & Authentication](#2-connection--authentication)
3. [Heartbeat](#3-heartbeat)
4. [Command Execution Flow](#4-command-execution-flow)
5. [VM Lifecycle Commands](#5-vm-lifecycle-commands)
6. [Port Forwarding Commands](#6-port-forwarding-commands)
7. [Console TTY](#7-console-tty)
8. [Keep-alive](#8-keep-alive)
9. [Reconnection](#9-reconnection)
10. [Code Generation](#10-code-generation)
11. [Minimal Go Example](#11-minimal-go-example)

---

## 1. Overview

```
┌─────────────────────────┐   gRPC bidirectional stream   ┌──────────────────────────┐
│  Agent (your impl)      │ ══════════════════════════════ │  Narwhal Cloud Platform  │
│  running on host        │   AgentEnvelope  →            │  gRPC Server             │
│                         │   ← PlatformEnvelope          │                          │
└─────────────────────────┘                               └──────────────────────────┘
```

- The **Agent dials the platform** gRPC endpoint; the platform acts as the server.
- Once connected, both sides communicate over a single **persistent bidirectional stream** — no new handshake per message.
- The Agent is responsible for the full lifecycle of every VM on its host (create, start/stop, delete, reinstall, etc.).
- The platform never dials the Agent; all control commands flow downstream through the stream.

**Protocol file**: [proto/agent/agent.proto](proto/agent/agent.proto)

---

## 2. Connection & Authentication

### 2.1 gRPC Endpoint

Obtain an `agent_token` and the gRPC address from the platform. The Agent connects using standard gRPC dial. TLS is recommended.

### 2.2 Authentication

Pass the token via **gRPC metadata** on every `Connect` call:

```
key:   authorization
value: Bearer <agent_token>
```

Go example:

```go
import "google.golang.org/grpc/metadata"

ctx := metadata.AppendToOutgoingContext(context.Background(),
    "authorization", "Bearer "+agentToken,
)
stream, err := client.Connect(ctx)
```

### 2.3 Opening the Stream

```go
stream, err := agentClient.Connect(ctx)
// stream is grpc.BidiStreamingClient[AgentEnvelope, PlatformEnvelope]
// Upstream:   stream.Send(&AgentEnvelope{...})
// Downstream: stream.Recv() returns *PlatformEnvelope
```

Send the first **Heartbeat** immediately after the stream opens, then every **30 seconds**.

---

## 3. Heartbeat

The Heartbeat is the primary mechanism for the Agent to report host state. The platform relies on it for reconciliation and anomaly detection.

### 3.1 Frequency

Send every **30 seconds**. Must not exceed 60 seconds between heartbeats.

### 3.2 Heartbeat Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `timestamp` | int64 | ✓ | Unix epoch seconds |
| `cpu_pct` | float | ✓ | Host CPU utilization, 0–100 |
| `ram_used_mb` | int64 | ✓ | Used RAM (MB) |
| `disk_used_gb` | int64 | ✓ | Used disk (GB) |
| `net_in_bps` | int64 | ✓ | Real-time inbound rate (bytes/s) |
| `net_out_bps` | int64 | ✓ | Real-time outbound rate (bytes/s) |
| `load1/5/15` | float | ✓ | System load averages |
| `uptime` | int64 | ✓ | System uptime (seconds) |
| `cpus` | int32 | ✓ | Logical CPU count |
| `ram_total_mb` | int64 | ✓ | Total RAM (MB) |
| `disk_total_gb` | int64 | ✓ | Total disk (GB) |
| `bandwidth_mbps` | int32 | ✓ | Total bandwidth (Mbps) |
| `virt_type` | string | ✓ | Virtualization backend: `podman` / `cloudhv` |
| `vms` | VMSummary[] | ✓ | Status summary of all VMs on this host |
| `entry_host` | string | on change | Host IPv4 address or DDNS hostname |
| `entry_ipv6` | string | on change | Host IPv6 address (optional) |
| `os_images` | OSImageInfo[] | on change | List of available OS images |

> `entry_host`, `entry_ipv6`, and `os_images` only need to be populated **when their values change**. Leave them empty otherwise; the platform retains the last reported value.

### 3.3 VMSummary Fields

| Field | Type | Description |
|-------|------|-------------|
| `vm_id` | string | VM UUID (assigned by the platform) |
| `status` | VMStatus | Current runtime status |
| `cpu_pct` | float | VM CPU utilization, 0–100 |
| `ram_used_mb` | int64 | VM used RAM (MB) |
| `traffic_in_bytes` | int64 | Cumulative inbound traffic (bytes, since creation) |
| `traffic_out_bytes` | int64 | Cumulative outbound traffic (bytes, since creation) |
| `ips` | string[] | VM IP addresses (internal and public) |
| `monthly_traffic_in` | int64 | Current month inbound traffic (bytes) |
| `monthly_traffic_out` | int64 | Current month outbound traffic (bytes) |

### 3.4 VMStatus Enum

| Value | Meaning |
|-------|---------|
| `VM_STATUS_CREATING` | Being provisioned (platform waiting for Agent report) |
| `VM_STATUS_RUNNING` | Running |
| `VM_STATUS_STOPPED` | Stopped / powered off |
| `VM_STATUS_ERROR` | Error state |

---

## 4. Command Execution Flow

Every platform command follows a **request-acknowledgement** pattern:

```
Platform                          Agent
   │                                │
   │── PlatformEnvelope ──────────►│  command with unique command_id
   │   command_id = "cmd-uuid-123" │
   │   payload = CmdStartVM{...}   │
   │                                │
   │                           execute
   │                                │
   │◄─ AgentEnvelope ───────────────│  report result
   │   payload = CommandResult{     │
   │     command_id = "cmd-uuid-123"│
   │     success = true             │
   │   }                            │
```

**Rules:**

1. Every `PlatformEnvelope` command **must** be acknowledged with a `CommandResult`, including failures.
2. `CommandResult.command_id` must exactly match the corresponding command's `command_id`.
3. **Exception: `Ping` requires no `CommandResult`.**
4. Commands may execute concurrently; acknowledgements need not be in order.

### 4.1 CommandResult Structure

```protobuf
message CommandResult {
  string command_id = 1; // echoes PlatformEnvelope.command_id
  bool   success    = 2; // whether the command succeeded
  string error      = 3; // error message when success=false
  bytes  data       = 4; // JSON-encoded extra data (command-specific)
}
```

### 4.2 AgentEnvelope Upstream Message Types

| oneof field | When to send |
|-------------|--------------|
| `heartbeat` | Every 30 seconds |
| `cmd_result` | After executing any command |
| `port_fwd_list` | In response to `CmdGetPortForwards` |
| `console_output` | When the VM TTY produces output |
| `console_event` | When a console session changes state |

---

## 5. VM Lifecycle Commands

### 5.1 CmdCreateVM — Create a VM

On receiving this command, the Agent should:

1. Provision and start a VM with the specified resources.
2. Report `VMStatus = VM_STATUS_RUNNING` in the next heartbeat.
3. Acknowledge with `CommandResult{success: true}`.

| Field | Type | Description |
|-------|------|-------------|
| `vm_id` | string | VM UUID — use as the local identifier |
| `cpu` | int32 | vCPU count |
| `ram_mb` | int64 | RAM (MB) |
| `disk_gb` | int64 | Disk (GB) |
| `bandwidth_mbps` | int32 | Bandwidth limit (Mbps); 0 = unlimited |
| `os_image` | string | OS image ID, must match an entry from `Heartbeat.os_images` |
| `root_password` | string | Initial root password (plaintext) |

### 5.2 CmdStartVM — Start a VM

Power on a stopped VM. Report `RUNNING` in the next heartbeat on success.

### 5.3 CmdStopVM — Stop a VM

| Field | Description |
|-------|-------------|
| `vm_id` | VM UUID |
| `force` | `true` = hard power cut; `false` = graceful shutdown (ACPI signal) |

### 5.4 CmdRestartVM — Restart a VM

Perform a graceful reboot. For a force restart, the platform sends `CmdStopVM{force:true}` followed by `CmdStartVM`.

### 5.5 CmdDeleteVM — Destroy a VM

**Irreversible.** Release all associated resources (disk image, IPs, port-forward rules, etc.).  
The platform completes billing settlement before issuing this command.

### 5.6 CmdReinstallVM — Reinstall OS

The existing disk is wiped. Recommended sequence: stop VM → destroy old instance → reprovision with new image → start.

| Field | Description |
|-------|-------------|
| `os_image` | New OS image ID |
| `root_password` | New root password |
| `cpu/ram_mb/disk_gb/bandwidth_mbps` | Resource spec (may change with plan upgrade/downgrade) |

### 5.7 CmdResetPassword — Reset Root Password

The Agent must write the new password into the VM (e.g. via cloud-init, `chroot` + `chpasswd`).  
The password is transmitted as plaintext — enable TLS at the transport layer.

---

## 6. Port Forwarding Commands

The Agent must maintain port-forwarding rules at the host network layer (e.g. `iptables`/`nftables`) to forward traffic from host ports to the corresponding VM's internal ports.

### 6.1 CmdSetPortForward — Add a Rule

| Field | Description |
|-------|-------------|
| `vm_id` | VM UUID |
| `protocol` | `PROTOCOL_TCP` or `PROTOCOL_UDP` |
| `host_port` | Host (external) port; recommended range 1024–65535 |
| `guest_port` | VM internal port |
| `description` | Label e.g. "SSH", "HTTP" |

### 6.2 CmdDelPortForward — Remove a Rule

Uniquely identifies the rule by the `{vm_id, protocol, host_port}` triple.

### 6.3 CmdGetPortForwards — List Rules

**Note:** The response does **not** use `CommandResult.data`. Send the result via `AgentEnvelope.port_fwd_list`, and also send a `CommandResult` to acknowledge the command:

```go
// Go example
stream.Send(&AgentEnvelope{
    MessageId: uuid.New().String(),
    Payload: &AgentEnvelope_PortFwdList{
        PortFwdList: &PortForwardList{
            CommandId: cmd.CommandId,
            Entries:   entries,
        },
    },
})
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

## 7. Console TTY

The console feature lets users interact directly with a VM's TTY via the platform's WebSocket interface.

### 7.1 Session Flow

```
Platform                                    Agent
   │                                          │
   │── CmdConsoleOpen ─────────────────────►│
   │   vm_id = "vm-uuid"                    │
   │   session_id = "sess-uuid"             │  ← unique session identifier
   │   cols = 220, rows = 50               │
   │                                          │
   │                              attach VM TTY (e.g. podman exec -it)
   │                                          │
   │◄─ ConsoleEvent{CONNECTED} ──────────────│  TTY attached
   │◄─── CommandResult{success:true} ────────│  command ack
   │                                          │
   │── CmdConsoleInput ──────────────────────►│
   │   session_id = "sess-uuid"             │
   │   data = <stdin bytes>                 │
   │                                          │
   │◄─ ConsoleOutput ────────────────────────│
   │   session_id = "sess-uuid"             │
   │   data = <stdout bytes>                │
   │                                          │
   │── CmdConsoleResize ─────────────────────►│
   │   cols = 240, rows = 60               │  terminal resize
   │                                          │
   │── CmdConsoleClose ──────────────────────►│
   │   session_id = "sess-uuid"             │
   │                                          │
   │◄─ ConsoleEvent{DISCONNECTED} ───────────│
   │◄─── CommandResult{success:true} ────────│
```

### 7.2 Message Reference

| Direction | Message | Description |
|-----------|---------|-------------|
| Downstream | `CmdConsoleOpen` | Request Agent to open a TTY session |
| Downstream | `CmdConsoleInput` | Forward raw keyboard input (including control chars) to VM stdin |
| Downstream | `CmdConsoleResize` | Resize terminal (`ioctl TIOCSWINSZ`) |
| Downstream | `CmdConsoleClose` | Close session; Agent cleans up TTY resources |
| Upstream | `ConsoleOutput` | Raw TTY output bytes from VM |
| Upstream | `ConsoleEvent` | Session lifecycle events (CONNECTED / DISCONNECTED / ERROR) |

### 7.3 Error Handling

- If the VM does not exist or the TTY cannot be attached, report `ConsoleEvent{type: ERROR, reason: "..."}` and `CommandResult{success: false, error: "..."}`.
- If the TTY disconnects mid-session (VM reboot, crash, etc.), proactively report `ConsoleEvent{type: DISCONNECTED, reason: "..."}` without waiting for `CmdConsoleClose`.

### 7.4 session_id

`session_id` is generated by the platform. All console frames (input, output, events, resize) for the same session carry the same `session_id`. A single host may have multiple concurrent console sessions across different VMs; the Agent must use `session_id` to route frames correctly.

---

## 8. Keep-alive

The platform periodically sends `Ping` messages to prevent proxies (Nginx, CDN, etc.) from closing idle connections.

**The Agent does not need to send any response to `Ping`, including `CommandResult`.**

```protobuf
message Ping {}  // empty — ignore it
```

You may also enable gRPC-level keepalive on the Agent side:

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

## 9. Reconnection

### 9.1 Reconnect Strategy

On any stream error, reconnect immediately using exponential backoff:

```
Attempt 1: wait 1 s
Attempt 2: wait 2 s
Attempt 3: wait 4 s
...
Cap:       60 s
```

### 9.2 Command Idempotency

The platform does **not** replay unacknowledged commands after reconnection. Commands missed during a disconnect will not be retransmitted. The Agent should reconnect quickly and restore the heartbeat so the platform can sense the node is back online.

That said, each command implementation should be idempotent to handle the rare case of a duplicate delivery:

- `CmdCreateVM`: if the VM already exists, return success.
- `CmdDeleteVM`: if the VM no longer exists, return success.
- `CmdSetPortForward`: if the rule already exists, overwrite or skip.
- `CmdDelPortForward`: if the rule does not exist, treat as success.

### 9.3 Message Deduplication

Each `AgentEnvelope` carries a `message_id` (UUID v4 recommended). The platform may use this field to deduplicate messages retransmitted due to network jitter.

---

## 10. Code Generation

Install `protoc` and the Go plugins, then run from the project root:

```bash
# Install plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Generate
protoc \
  --go_out=. \
  --go_opt=paths=source_relative \
  --go-grpc_out=. \
  --go-grpc_opt=paths=source_relative \
  proto/agent/agent.proto
```

Other languages (Python, Rust, TypeScript, etc.) follow the same pattern — substitute the appropriate `protoc` plugin.

---

## 11. Minimal Go Example

The snippet below demonstrates the core connection loop; VM management details are omitted.

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

    // Start heartbeat goroutine
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        sendHeartbeat(stream) // send immediately
        for range ticker.C {
            if err := sendHeartbeat(stream); err != nil {
                return
            }
        }
    }()

    // Receive and dispatch platform commands
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
        // no response needed

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

    // ... remaining command types
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
        // ... populate remaining fields
        Vms: getVMSummaries(),
    }
    return stream.Send(&pb.AgentEnvelope{
        MessageId: newUUID(),
        Payload:   &pb.AgentEnvelope_Heartbeat{Heartbeat: hb},
    })
}
```

---

*Protocol version: v1.0 — refer to the version header in [agent.proto](proto/agent/agent.proto) for updates. Narwhal Cloud © 2026*
