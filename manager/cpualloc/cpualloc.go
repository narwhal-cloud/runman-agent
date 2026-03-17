package cpualloc

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Allocator 使用引用计数将宿主机 CPU 分配给容器。
// 允许超售：多个容器可共享同一物理 CPU，内核 CFS quota 保证各自的 CPU 时间。
// 分配策略：每次优先选用当前引用数最少的核，使负载均匀分布在所有物理 CPU 上。
type Allocator struct {
	mu   sync.Mutex
	refs []int // refs[i] = 当前使用 CPU i 的容器数
}

// New 创建分配器，total 为宿主机逻辑 CPU 总数。
func New(total int) *Allocator {
	return &Allocator{refs: make([]int, total)}
}

// HostCPUCount 从 /sys/devices/system/cpu/online 读取在线 CPU 数。
func HostCPUCount() int {
	data, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 1
	}
	return countCPUs(strings.TrimSpace(string(data)))
}

// Restore 在 Agent 重启时，从已持久化的 cpuset 字符串恢复引用计数。
// 对 DB 中记录的每个 VM 调用一次。
func (a *Allocator) Restore(cpuset string) {
	if cpuset == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cpu := range parseCpuset(cpuset) {
		if cpu < len(a.refs) {
			a.refs[cpu]++
		}
	}
}

// Allocate 挑选 n 个引用数最小的 CPU，递增它们的引用计数，
// 返回 cpuset 字符串（如 "0-2" 或 "0,3,5"）。
func (a *Allocator) Allocate(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	total := len(a.refs)
	if total == 0 {
		return "", fmt.Errorf("no CPUs available")
	}
	if n > total {
		n = total
	}
	chosen := pickLeast(a.refs, n)
	for _, cpu := range chosen {
		a.refs[cpu]++
	}
	return formatCpuset(chosen), nil
}

// Release 释放之前分配的 cpuset，递减对应 CPU 的引用计数。
func (a *Allocator) Release(cpuset string) {
	if cpuset == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cpu := range parseCpuset(cpuset) {
		if cpu < len(a.refs) && a.refs[cpu] > 0 {
			a.refs[cpu]--
		}
	}
}

// pickLeast 返回 refs 中值最小的 n 个索引，相同引用数时优先选索引较小的 CPU。
func pickLeast(refs []int, n int) []int {
	type pair struct{ cpu, ref int }
	pairs := make([]pair, len(refs))
	for i, r := range refs {
		pairs[i] = pair{i, r}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].ref != pairs[j].ref {
			return pairs[i].ref < pairs[j].ref
		}
		return pairs[i].cpu < pairs[j].cpu
	})
	result := make([]int, 0, n)
	for i := 0; i < n; i++ {
		result = append(result, pairs[i].cpu)
	}
	return result
}

// parseCpuset 将 "0,2,4-6" 解析为 [0, 2, 4, 5, 6]。
func parseCpuset(s string) []int {
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if idx := strings.Index(part, "-"); idx >= 0 {
			lo, _ := strconv.Atoi(part[:idx])
			hi, _ := strconv.Atoi(part[idx+1:])
			for i := lo; i <= hi; i++ {
				cpus = append(cpus, i)
			}
		} else if cpu, err := strconv.Atoi(part); err == nil {
			cpus = append(cpus, cpu)
		}
	}
	return cpus
}

// formatCpuset 将 [0,1,2,5] 格式化为 "0-2,5"。
func formatCpuset(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	sort.Ints(cpus)
	var parts []string
	start, end := cpus[0], cpus[0]
	flush := func() {
		if start == end {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, end))
		}
	}
	for _, cpu := range cpus[1:] {
		if cpu == end+1 {
			end = cpu
		} else {
			flush()
			start, end = cpu, cpu
		}
	}
	flush()
	return strings.Join(parts, ",")
}

// countCPUs 计算 cpuset 字符串中的 CPU 总数。
func countCPUs(s string) int {
	count := 0
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if idx := strings.Index(part, "-"); idx >= 0 {
			lo, _ := strconv.Atoi(part[:idx])
			hi, _ := strconv.Atoi(part[idx+1:])
			count += hi - lo + 1
		} else if part != "" {
			count++
		}
	}
	return count
}
