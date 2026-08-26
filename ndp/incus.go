//go:build linux

package ndp

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"time"

	"go4.org/netipx"
)

// incusTracker monitors Incus VM IPv6 addresses in the database
type incusTracker struct {
	db     interface{} // *db.DB
	ips    *netipx.IPSet
	mu     sync.RWMutex
	newIPs chan netip.Addr
}

func newIncusTracker(db interface{}) *incusTracker {
	if db == nil {
		return nil
	}
	dbVal := reflect.ValueOf(db)
	if dbVal.Kind() == reflect.Ptr && dbVal.MethodByName("ListIncusConfigs").IsValid() {
		return &incusTracker{
			db:     db,
			ips:    &netipx.IPSet{},
			newIPs: make(chan netip.Addr, 64),
		}
	}
	return nil
}

func (t *incusTracker) run(ctx context.Context) {
	if t == nil {
		return
	}
	t.sync()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sync()
		}
	}
}

func (t *incusTracker) sync() {
	if t.db == nil {
		return
	}

	dbVal := reflect.ValueOf(t.db)
	method := dbVal.MethodByName("ListIncusConfigs")
	if !method.IsValid() {
		return
	}

	results := method.Call(nil)
	if len(results) < 2 || !results[1].IsNil() {
		return
	}

	configsVal := results[0]
	var b netipx.IPSetBuilder
	var newIPsList []netip.Addr
	seen := make(map[netip.Addr]struct{})

	if configsVal.Kind() == reflect.Slice {
		for i := 0; i < configsVal.Len(); i++ {
			cfg := configsVal.Index(i)
			if cfg.Kind() == reflect.Ptr {
				if cfg.IsNil() {
					continue
				}
				cfg = cfg.Elem()
			}

			// 新记录把完整地址列表保存在 IPv6s，旧记录可能只存在
			// IPv6（或把逗号列表写在其中）；兼容两种格式并去重。
			var values []string
			for _, fieldName := range []string{"IPv6", "IPv6s"} {
				field := cfg.FieldByName(fieldName)
				if field.IsValid() && field.Kind() == reflect.String {
					values = append(values, strings.Split(field.String(), ",")...)
				}
			}
			for _, entry := range values {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				ip, err := netip.ParseAddr(entry)
				if err != nil {
					continue
				}
				if _, exists := seen[ip]; exists {
					continue
				}
				seen[ip] = struct{}{}
				b.Add(ip)

				t.mu.RLock()
				isNew := !t.ips.Contains(ip)
				t.mu.RUnlock()
				if isNew {
					newIPsList = append(newIPsList, ip)
				}
			}
		}
	}

	newIPs, _ := b.IPSet()
	t.mu.Lock()
	t.ips = newIPs
	t.mu.Unlock()

	for _, ip := range newIPsList {
		select {
		case t.newIPs <- ip:
		default:
		}
	}
}

func (t *incusTracker) contains(ip netip.Addr) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ips.Contains(ip)
}

func (t *incusTracker) allIPs() []netip.Addr {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var ips []netip.Addr
	for _, p := range t.ips.Prefixes() {
		if p.IsSingleIP() {
			ips = append(ips, p.Addr())
		}
	}
	return ips
}
