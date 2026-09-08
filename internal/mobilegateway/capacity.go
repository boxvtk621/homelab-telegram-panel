package mobilegateway

import "sync"

const (
	maximumGeneralInFlight  = 24
	maximumReservedInFlight = 2
	maximumBusinessInFlight = maximumGeneralInFlight + 4*maximumReservedInFlight
)

// CapacityClass is the frozen W-MOBILE request classification. Reserved
// classes cannot borrow from general capacity or from one another.
type CapacityClass uint8

const (
	CapacityGeneral CapacityClass = iota + 1
	CapacityHealth
	CapacityStatus
	CapacityCancel
	CapacityRecovery
)

// CapacityGate enforces exactly 24 general plus four isolated two-slot
// reserves. It has no wait queue and therefore cannot grow under overload.
type CapacityGate struct {
	mu       sync.Mutex
	inFlight map[CapacityClass]int
}

// CapacitySnapshot is the public-safe aggregate of the Gateway's own bounded
// admission state. Individual reserve classes are deliberately not exposed.
type CapacitySnapshot struct {
	GeneralInFlight  int
	GeneralLimit     int
	ReservedInFlight int
	ReservedLimit    int
}

// NewCapacityGate constructs the one process-wide Gateway admission gate.
func NewCapacityGate() *CapacityGate {
	return &CapacityGate{inFlight: make(map[CapacityClass]int, 5)}
}

// Acquire admits immediately or fails closed. Call release exactly once.
func (gate *CapacityGate) Acquire(class CapacityClass) (release func(), admitted bool) {
	if gate == nil {
		return nil, false
	}
	limit := capacityLimit(class)
	if limit == 0 {
		return nil, false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.inFlight == nil || gate.inFlight[class] >= limit {
		return nil, false
	}
	gate.inFlight[class]++
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			gate.inFlight[class]--
			gate.mu.Unlock()
		})
	}, true
}

// Snapshot atomically reports the canonical 24 general plus aggregate 8
// reserved Gateway slots. It never contains Controller/runtime capacity.
func (gate *CapacityGate) Snapshot() CapacitySnapshot {
	if gate == nil {
		return CapacitySnapshot{}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reserved := 0
	for _, class := range []CapacityClass{CapacityHealth, CapacityStatus, CapacityCancel, CapacityRecovery} {
		reserved += gate.inFlight[class]
	}
	return CapacitySnapshot{
		GeneralInFlight:  gate.inFlight[CapacityGeneral],
		GeneralLimit:     maximumGeneralInFlight,
		ReservedInFlight: reserved,
		ReservedLimit:    4 * maximumReservedInFlight,
	}
}

func capacityLimit(class CapacityClass) int {
	if class == CapacityGeneral {
		return maximumGeneralInFlight
	}
	switch class {
	case CapacityHealth, CapacityStatus, CapacityCancel, CapacityRecovery:
		return maximumReservedInFlight
	default:
		return 0
	}
}
