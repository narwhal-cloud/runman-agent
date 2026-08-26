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

	if configsVal.Kind() == reflect.Slice {
		for i := 0; i < configsVal.Len(); i++ {
			cfg := configsVal.Index(i)
			if cfg.Kind() == reflect.Ptr {
				if cfg.IsNil() {
					continue
				}
				cfg = cfg.Elem()
			}

			ipv6Field := cfg.FieldByName("IPv6")
			if !ipv6Field.IsValid() {
				continue
			}

			ipv6 := ipv6Field.String()
			if ipv6 == "" {
				continue
			}

			// IPv6 字段可能是逗号分隔的多个地址（非 /64 网段精细化分配）
			for _, entry := range strings.Split(ipv6, ",") {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				ip, err := netip.ParseAddr(entry)
				if err != nil {
					continue
				}
				b.Add(ip)
			}

			t.mu.RLock()
			isNew := !t.ips.Contains(ip)
			t.mu.RUnlock()
			if isNew {
				newIPsList = append(newIPsList, ip)
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
