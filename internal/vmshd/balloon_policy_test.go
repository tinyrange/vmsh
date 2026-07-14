package vmshd

import (
	"testing"

	"j5.nz/cc/client"
)

type fakeMemoryObserver struct {
	snapshot memorySnapshot
	err      error
}

func (f fakeMemoryObserver) Snapshot() (memorySnapshot, error) {
	return f.snapshot, f.err
}

func TestDefaultGuestMemoryTracksHostSize(t *testing.T) {
	tests := []struct {
		hostMB uint64
		want   uint64
	}{
		{hostMB: 4096, want: 2048},
		{hostMB: 8192, want: 4096},
		{hostMB: 12288, want: 8192},
		{hostMB: 16384, want: 12288},
		{hostMB: 24576, want: 16384},
	}
	for _, tt := range tests {
		if got := defaultGuestMemoryMB(tt.hostMB); got != tt.want {
			t.Fatalf("defaultGuestMemoryMB(%d) = %d, want %d", tt.hostMB, got, tt.want)
		}
	}
}

func TestBalloonPolicySimulationMaintainsReserveAcrossPressureAndRecovery(t *testing.T) {
	cfg := balloonPolicyConfig{ReservePercent: 10, StepMB: 128, HysteresisMB: 256, MinUsableMB: 1024}
	vms := []balloonVM{
		{ID: "a", ConfiguredMB: 4096, Eligible: true},
		{ID: "b", ConfiguredMB: 4096, Eligible: true},
		{ID: "c", ConfiguredMB: 4096, Eligible: true},
	}
	available := uint64(256)
	total := uint64(8192)

	for i := 0; i < 8; i++ {
		decisions := planBalloonTargets(memorySnapshot{TotalMB: total, AvailableMB: available}, vms, cfg)
		delta := applyDecisions(vms, decisions)
		available += delta
	}
	if available < 819 {
		t.Fatalf("available after pressure response = %d, want at least 819", available)
	}
	assertBalancedTargets(t, vms, 128)

	available = 2048
	for i := 0; i < 8; i++ {
		decisions := planBalloonTargets(memorySnapshot{TotalMB: total, AvailableMB: available}, vms, cfg)
		delta := applyDecisions(vms, decisions)
		available += delta
	}
	for _, vm := range vms {
		if vm.BalloonMB != 0 {
			t.Fatalf("balloons after recovery = %+v, want all deflated", vms)
		}
	}
}

func TestBalloonPolicySimulationRespectsMinimumUsableMemory(t *testing.T) {
	cfg := balloonPolicyConfig{ReservePercent: 10, StepMB: 256, HysteresisMB: 256, MinUsableMB: 3072}
	vms := []balloonVM{
		{ID: "small", ConfiguredMB: 2048, Eligible: true},
		{ID: "large", ConfiguredMB: 4096, Eligible: true},
	}
	available := uint64(0)
	for i := 0; i < 8; i++ {
		decisions := planBalloonTargets(memorySnapshot{TotalMB: 8192, AvailableMB: available}, vms, cfg)
		available += applyDecisions(vms, decisions)
	}
	if vms[0].BalloonMB != 0 {
		t.Fatalf("small VM balloon = %d, want 0 because configured memory is below floor", vms[0].BalloonMB)
	}
	if vms[1].BalloonMB != 819 {
		t.Fatalf("large VM balloon = %d, want reserve-sized target 819", vms[1].BalloonMB)
	}
}

func TestBalloonPolicySimulationUsesExistingTargets(t *testing.T) {
	cfg := balloonPolicyConfig{ReservePercent: 10, StepMB: 128, HysteresisMB: 256, MinUsableMB: 1024}
	vms := []balloonVM{
		{ID: "a", ConfiguredMB: 4096, BalloonMB: 512, Eligible: true},
		{ID: "b", ConfiguredMB: 4096, BalloonMB: 512, Eligible: true},
	}
	decisions := planBalloonTargets(memorySnapshot{TotalMB: 10240, AvailableMB: 2048}, vms, cfg)
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want one decision per VM", decisions)
	}
	for _, decision := range decisions {
		if decision.BalloonMB != 128 {
			t.Fatalf("decision = %+v, want policy to deflate from current target", decision)
		}
	}
}

func TestBalloonVMsFromInstancesCarriesCurrentTarget(t *testing.T) {
	vms := balloonVMsFromInstances([]client.InstanceState{
		{ID: "running", Status: "running", MemoryMB: 4096, BalloonMB: 768},
		{ID: "stopped", Status: "stopped", MemoryMB: 4096, BalloonMB: 768},
	})
	if len(vms) != 1 {
		t.Fatalf("vms = %+v, want one running VM", vms)
	}
	if vms[0].ID != "running" || vms[0].ConfiguredMB != 4096 || vms[0].BalloonMB != 768 {
		t.Fatalf("vm = %+v, want running VM memory and balloon target", vms[0])
	}
}

func TestRuntimePolicyAppliesDefaultMemoryAndInitialBalloon(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 4096}})
	runtime := fakeRuntimeView{statuses: []client.InstanceState{
		{ID: "existing", Status: "running", MemoryMB: 4096},
	}}

	req := client.StartInstanceRequest{ID: "new", Image: "alpine"}
	srv.normalizeStartRequest(&req, runtime)
	if req.MemoryMB != 4096 {
		t.Fatalf("memory_mb = %d, want 4096", req.MemoryMB)
	}
	if req.BalloonMB == 0 {
		t.Fatalf("balloon_mb = %d, want pressure-derived initial target", req.BalloonMB)
	}
}

func TestRuntimePolicyPreservesExplicitMemoryAndBalloon(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})

	req := client.RunRequest{Image: "alpine", MemoryMB: 2048, BalloonMB: 256}
	srv.normalizeRunRequest(&req, nil)
	if req.MemoryMB != 2048 || req.BalloonMB != 256 {
		t.Fatalf("request = %+v, want explicit memory and balloon preserved", req)
	}
}

func applyDecisions(vms []balloonVM, decisions []balloonDecision) uint64 {
	var reclaimed uint64
	index := make(map[string]int, len(vms))
	for i, vm := range vms {
		index[vm.ID] = i
	}
	for _, decision := range decisions {
		i, ok := index[decision.ID]
		if !ok {
			continue
		}
		if decision.BalloonMB >= vms[i].BalloonMB {
			reclaimed += decision.BalloonMB - vms[i].BalloonMB
		}
		vms[i].BalloonMB = decision.BalloonMB
	}
	return reclaimed
}

func assertBalancedTargets(t *testing.T, vms []balloonVM, tolerance uint64) {
	t.Helper()
	var minTarget, maxTarget uint64
	for i, vm := range vms {
		if i == 0 || vm.BalloonMB < minTarget {
			minTarget = vm.BalloonMB
		}
		if vm.BalloonMB > maxTarget {
			maxTarget = vm.BalloonMB
		}
	}
	if maxTarget-minTarget > tolerance {
		t.Fatalf("balloon targets = %+v, want spread within %d MB", vms, tolerance)
	}
}
