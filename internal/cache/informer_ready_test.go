// Q-PREWARM-R2 — informer-ready signal tests.
package cache

import (
	"sync"
	"testing"
)

func TestInformerReady_DefaultFalse(t *testing.T) {
	ResetInformersReadyForTest()
	if IsInformerReady() {
		t.Fatalf("expected IsInformerReady() == false at startup")
	}
}

func TestInformerReady_MarkFlipsToTrue(t *testing.T) {
	ResetInformersReadyForTest()
	if IsInformerReady() {
		t.Fatalf("precondition: expected false")
	}
	MarkInformersReady()
	if !IsInformerReady() {
		t.Fatalf("expected IsInformerReady() == true after MarkInformersReady()")
	}
	ResetInformersReadyForTest()
}

func TestInformerReady_Idempotent(t *testing.T) {
	ResetInformersReadyForTest()
	MarkInformersReady()
	MarkInformersReady()
	MarkInformersReady()
	if !IsInformerReady() {
		t.Fatalf("expected idempotent MarkInformersReady to leave flag true")
	}
	ResetInformersReadyForTest()
}

// TestInformerReady_ConcurrentSafe verifies the atomic semantics under
// the same kind of concurrent access the production /ready handler
// experiences (kubelet probes interleaving with the single
// MarkInformersReady call from main).
func TestInformerReady_ConcurrentSafe(t *testing.T) {
	ResetInformersReadyForTest()

	const N = 256
	var wg sync.WaitGroup
	wg.Add(N + 1)

	// Many concurrent readers (probe handlers).
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = IsInformerReady()
			}
		}()
	}

	// One concurrent writer (the post-WaitForSync hook).
	go func() {
		defer wg.Done()
		MarkInformersReady()
	}()

	wg.Wait()
	if !IsInformerReady() {
		t.Fatalf("expected true after MarkInformersReady completed")
	}
	ResetInformersReadyForTest()
}
