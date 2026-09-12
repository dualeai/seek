package main

import (
	"context"
	"runtime"
	"sync"
	"time"
)

// searchResources supplies the effective Go CPU allowance and available-memory
// probe for one command. Semantic extraction, CPU-provider model calls, and
// USearch writers use its CPU gate. The gate and inference controller read CPU
// changes while work is queued. Zoekt, USearch, and tokenizer worker pools take
// a cpuLimit snapshot when each pool starts.
type searchResources struct {
	now             func() time.Time
	effectiveCPUs   func() int
	availableMemory func() int64
	// callCPUs is the estimated gate reservation for one model call. Zero skips
	// CPU-gate admission for an accelerated provider; it does not assert that the
	// provider uses no physical CPU.
	callCPUs int
	cpu      *dynamicCPUGate
}

func liveSearchResources() searchResources {
	resources := searchResources{
		now:             time.Now,
		effectiveCPUs:   func() int { return runtime.GOMAXPROCS(0) },
		availableMemory: semanticHostAvailableMemory,
	}
	resources.cpu = newDynamicCPUGate(resources.cpuLimit)
	return resources
}

func (resources searchResources) cpuLimit() int {
	if resources.effectiveCPUs == nil {
		return max(1, runtime.GOMAXPROCS(0))
	}
	return max(1, resources.effectiveCPUs())
}

func (resources searchResources) currentTime() time.Time {
	if resources.now == nil {
		return time.Now()
	}
	return resources.now()
}

func (resources searchResources) freeMemory() int64 {
	if resources.availableMemory == nil {
		return semanticHostAvailableMemory()
	}
	return resources.availableMemory()
}

func (resources searchResources) withModelCallCost(callCPUs int) searchResources {
	resources.callCPUs = max(0, callCPUs)
	return resources
}

func (resources searchResources) callLimit() int {
	return semanticModelCallLimit(resources.cpuLimit(), resources.callCPUs)
}

func (resources searchResources) acquireCPU(ctx context.Context, cost int) (cpuLease, error) {
	if resources.cpu == nil || cost <= 0 {
		return cpuLease{}, nil
	}
	return resources.cpu.acquire(ctx, cost)
}

type cpuLease struct {
	gate *dynamicCPUGate
	cost int
}

func (lease cpuLease) release() {
	if lease.gate != nil {
		lease.gate.release(lease.cost)
	}
}

// dynamicCPUGate tracks declared CPU reservations across semantic extraction,
// CPU-provider model calls, and USearch writers. It reads the command CPU limit
// again while work is queued. One admitted task can cross the limit by its own
// bounded cost so that partial free capacity does not leave the host idle.
type dynamicCPUGate struct {
	mu      sync.Mutex
	used    int
	changed chan struct{}
	limit   func() int
}

func newDynamicCPUGate(limit func() int) *dynamicCPUGate {
	return &dynamicCPUGate{changed: make(chan struct{}), limit: limit}
}

func (gate *dynamicCPUGate) acquire(ctx context.Context, cost int) (cpuLease, error) {
	cost = max(1, cost)
	var retry *time.Ticker
	for {
		gate.mu.Lock()
		limit := max(1, gate.limit())
		if gate.used < limit {
			// Let one last task fill partial capacity. A CPU model call can use
			// more threads than the remaining count; the throughput controller
			// measures that bounded wider probe and keeps it only when faster.
			gate.used += cost
			gate.mu.Unlock()
			if retry != nil {
				retry.Stop()
			}
			return cpuLease{gate: gate, cost: cost}, nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		if retry == nil {
			retry = time.NewTicker(100 * time.Millisecond)
		}

		select {
		case <-ctx.Done():
			retry.Stop()
			return cpuLease{}, ctx.Err()
		case <-changed:
		case <-retry.C:
		}
	}
}

func (gate *dynamicCPUGate) release(cost int) {
	gate.mu.Lock()
	gate.used -= cost
	close(gate.changed)
	gate.changed = make(chan struct{})
	gate.mu.Unlock()
}
