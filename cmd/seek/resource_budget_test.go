package main

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestDynamicCPUGateUsesNewCapacity holds the one CPU of the gate, so a second
// acquire waits. When the live limit grows to 2, the waiting acquire must read
// it again and pass. The test runs in a synctest bubble: synctest.Wait returns
// when the second acquire waits, and the gate's retry timer runs on fake time.
func TestDynamicCPUGateUsesNewCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()
		select {
		case lease := <-admitted:
			lease.release()
			t.Fatal("gate exceeded its first CPU limit")
		default:
		}

		limit.Store(2)
		// A second of fake time holds several retries of the waiting acquire.
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case lease := <-admitted:
			lease.release()
		default:
			t.Fatal("gate did not use the larger live CPU limit")
		}
	})
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
