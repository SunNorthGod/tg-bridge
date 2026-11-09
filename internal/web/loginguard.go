package web

import (
	"sync"
	"time"
)

// The panel is exposed on the open internet, so automated password
// spraying against /login is expected. Counting failures per IP and
// banning the source keeps such scans cheap to ignore without locking
// out the legitimate user (the password check still runs first).
const (
	failWindow  = 15 * time.Minute
	failLimit   = 10
	banDuration = time.Hour
)

type ipRecord struct {
	fails    int
	lastFail time.Time
	banUntil time.Time
}

// loginGuard tracks failed panel logins per source IP. In-memory only:
// a restart lifts bans, which is acceptable for scan noise.
type loginGuard struct {
	mu  sync.Mutex
	ips map[string]*ipRecord
}

func newLoginGuard() *loginGuard {
	return &loginGuard{ips: make(map[string]*ipRecord)}
}

// allow reports whether the IP may attempt a login right now.
func (g *loginGuard) allow(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.ips[ip]
	if !ok {
		return true
	}
	if time.Now().Before(rec.banUntil) {
		return false
	}
	if time.Since(rec.lastFail) > failWindow {
		delete(g.ips, ip)
	}
	return true
}

// fail records one wrong password and bans the IP once the limit is
// reached within the window.
func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.ips[ip]
	if rec == nil {
		rec = &ipRecord{}
		g.ips[ip] = rec
	}
	if time.Since(rec.lastFail) > failWindow {
		rec.fails = 0
	}
	rec.fails++
	rec.lastFail = time.Now()
	if rec.fails >= failLimit {
		rec.banUntil = time.Now().Add(banDuration)
	}
}

// clear forgets the IP after a successful login.
func (g *loginGuard) clear(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.ips, ip)
}
