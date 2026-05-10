//go:build linux

package ndp

import (
	"context"
	"log"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"go4.org/netipx"
)

// cloudhvTracker monitors cloud-hypervisor VM IPv6 addresses in the database
// Uses reflection to access db.DB methods to avoid circular imports
type cloudhvTracker struct {
	db     interface{} // *db.DB
	ips    *netipx.IPSet
	mu     sync.RWMutex
	newIPs chan netip.Addr
}

func newCloudHVTracker(db interface{}) *cloudhvTracker {
	if db == nil {
		return nil
	}
	// Just check that it has the method we need
	dbVal := reflect.ValueOf(db)
	if dbVal.Kind() == reflect.Ptr && dbVal.MethodByName("ListCloudHVConfigs").IsValid() {
		return &cloudhvTracker{
			db:     db,
			ips:    &netipx.IPSet{},
			newIPs: make(chan netip.Addr, 64),
		}
	}
	log.Printf("NDP cloudhvTracker: ListCloudHVConfigs method not found on db")
	return nil
}

// run periodically syncs VM IPv6 addresses from the database
func (t *cloudhvTracker) run(ctx context.Context) {
	if t == nil {
		return
	}
	t.sync() // initial sync

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

// sync reads all cloud-hypervisor VM IPv6 addresses from the database
func (t *cloudhvTracker) sync() {
	if t.db == nil {
		return
	}

	// Call ListCloudHVConfigs via reflection
	dbVal := reflect.ValueOf(t.db)
	method := dbVal.MethodByName("ListCloudHVConfigs")
	if !method.IsValid() {
		log.Printf("CloudHV tracker: ListCloudHVConfigs method not found")
		return
	}

	results := method.Call(nil)
	if len(results) < 2 {
		return
	}

	configsVal := results[0]
	errVal := results[1]

	if !errVal.IsNil() {
		return
	}

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

			// Access IPv6 field
			ipv6Field := cfg.FieldByName("IPv6")
			if !ipv6Field.IsValid() {
				continue
			}

			ipv6 := ipv6Field.String()
			if ipv6 == "" {
				continue
			}

			ip, err := netip.ParseAddr(ipv6)
			if err != nil {
				continue
			}
			b.Add(ip)

			t.mu.RLock()
			isNew := !t.ips.Contains(ip)
			t.mu.RUnlock()
			if isNew {
				newIPsList = append(newIPsList, ip)
			}

		}
	}

	newIPs, err := b.IPSet()
	if err != nil {
		return
	}

	t.mu.Lock()
	t.ips = newIPs
	t.mu.Unlock()

	for _, ip := range newIPsList {
		select {
		case t.newIPs <- ip:
		default:
		}
	}

	if len(newIPsList) > 0 {
		log.Printf("[NDP] CloudHV: %d new IPv6 IPs", len(newIPsList))
	}
}

func (t *cloudhvTracker) contains(ip netip.Addr) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ips.Contains(ip)
}

func (t *cloudhvTracker) allIPs() []netip.Addr {
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
