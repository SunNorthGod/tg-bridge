package web

import (
	"testing"
	"time"
)

func TestLoginGuard(t *testing.T) {
	g := newLoginGuard()
	const ip = "203.0.113.9"

	if !g.allow(ip) {
		t.Fatal("fresh ip must be allowed")
	}
	for i := 0; i < failLimit-1; i++ {
		g.fail(ip)
	}
	if !g.allow(ip) {
		t.Fatal("ip below the limit must stay allowed")
	}
	g.fail(ip)
	if g.allow(ip) {
		t.Fatal("ip must be banned after reaching the limit")
	}

	// An expired ban lifts the block.
	g.ips[ip].banUntil = time.Now().Add(-time.Minute)
	if !g.allow(ip) {
		t.Fatal("expired ban must lift")
	}

	// A success clears the history.
	g.clear(ip)
	if !g.allow(ip) {
		t.Fatal("cleared ip must be allowed")
	}
	if _, ok := g.ips[ip]; ok {
		t.Fatal("record should be deleted after clear")
	}
}

func TestLoginGuardWindowReset(t *testing.T) {
	g := newLoginGuard()
	const ip = "203.0.113.10"
	for i := 0; i < failLimit; i++ {
		g.fail(ip)
	}
	// Age the failures past the window: the counter resets, ban aside.
	rec := g.ips[ip]
	rec.lastFail = time.Now().Add(-failWindow - time.Minute)
	rec.banUntil = time.Time{}
	if !g.allow(ip) {
		t.Fatal("old failures must not keep the ip blocked")
	}
}
