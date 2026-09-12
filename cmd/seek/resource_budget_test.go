package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestDynamicCPUGateUsesNewCapacity(t *testing.T) {
	var limit atomic.Int32
	limit.Store(1)
	gate := newDynamicCPUGate(func() int { return int(limit.Load()) })
	first, err := gate.acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	admitted := make(chan cpuLease, 1)
	go func() {
		lease, acquireErr := gate.acquire(t.Context(), 1)
		if acquireErr == nil {
			admitted <- lease
		}
	}()

	select {
	case lease := <-admitted:
		lease.release()
		t.Fatal("gate exceeded its first CPU limit")
	case <-time.After(20 * time.Millisecond):
	}

	limit.Store(2)
	select {
	case lease := <-admitted:
		lease.release()
	case <-time.After(time.Second):
		t.Fatal("gate did not use the larger live CPU limit")
	}
}

func TestSearchResourcesDoNotChargeAcceleratorWait(t *testing.T) {
	gate := newDynamicCPUGate(func() int { return 1 })
	resources := searchResources{cpu: gate}
	occupied, err := resources.acquireCPU(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.release()

	accelerated, err := resources.acquireCPU(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	accelerated.release()
}

func TestDynamicCPUGateFillsPartialCapacity(t *testing.T) {
	gate := newDynamicCPUGate(func() int { return 4 })
	first, err := gate.acquire(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	last, err := gate.acquire(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer last.release()
	if last.cost != 3 {
		t.Fatalf("last reservation=%d, want the full model-call cost", last.cost)
	}
}
