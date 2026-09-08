package mobilegateway

import (
	"sync"
	"time"
)

const (
	authRateWindow     = time.Minute
	bootstrapRateLimit = 240
	exchangeRateLimit  = 240
	revokeRateLimit    = 60
)

type authRoute uint8

const (
	authRouteBootstrap authRoute = iota + 1
	authRouteExchange
	authRouteRevoke
)

type rateWindow struct {
	startedAt time.Time
	used      int
}

// authGate is the fixed process-wide CPU admission rate. It deliberately does
// not trust X-Forwarded-For or other proxy-supplied identity. The bounded
// bootstrap store replaces per-client tokens and evicts oldest entries, while
// the shared CapacityGate owns the global 24+8 in-flight contract.
type authGate struct {
	mu      sync.Mutex
	windows map[authRoute]rateWindow
}

func newAuthGate() *authGate {
	return &authGate{windows: make(map[authRoute]rateWindow)}
}

func (gate *authGate) admit(route authRoute, now time.Time) bool {
	if gate == nil {
		return false
	}
	limit := rateLimit(route)
	if limit == 0 {
		return false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	window := gate.windows[route]
	if window.startedAt.IsZero() || now.Sub(window.startedAt) >= authRateWindow {
		window = rateWindow{startedAt: now}
	}
	// A backwards clock step never resets or expands the budget.
	if now.Before(window.startedAt) || window.used >= limit {
		return false
	}
	window.used++
	gate.windows[route] = window
	return true
}

func rateLimit(route authRoute) int {
	switch route {
	case authRouteBootstrap:
		return bootstrapRateLimit
	case authRouteExchange:
		return exchangeRateLimit
	case authRouteRevoke:
		return revokeRateLimit
	default:
		return 0
	}
}
