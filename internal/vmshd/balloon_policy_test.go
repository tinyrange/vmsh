package vmshd

import (
	"errors"
	"testing"
	"time"

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
		if decision.BalloonMB != 384 {
			t.Fatalf("decision = %+v, want one acknowledged policy step", decision)
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
	if !supportsAutomaticBalloon(req.Image) {
		if req.MemoryMB != defaultFixedGuestMemoryMB || req.BalloonMB != 0 || srv.balloon.isAutomatic(req.ID) {
			t.Fatalf("unsupported-host request = %+v policy=%+v", req, srv.balloon.state(req.ID))
		}
		return
	}
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

func TestRuntimePolicyDoesNotClassifyExplicitMemoryAsAutomatic(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	req := client.StartInstanceRequest{ID: "explicit-memory", MemoryMB: 2048}
	srv.normalizeStartRequest(&req, nil)
	if req.MemoryMB != 2048 || req.BalloonMB != 0 {
		t.Fatalf("explicit request changed to %+v", req)
	}
	if srv.balloon.isAutomatic(req.ID) {
		t.Fatal("explicit memory request was enrolled in automatic ballooning")
	}
}

func TestExistingInstanceRunDoesNotMutateMemoryPolicy(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	srv.balloon.setAutomatic("explicit", false)
	req := client.RunRequest{ID: "explicit", Command: []string{"true"}}
	srv.normalizeRunRequest(&req, fakeRuntimeView{statuses: []client.InstanceState{{ID: "explicit", Status: "running", MemoryMB: 2048}}})
	if req.MemoryMB != 0 || req.BalloonMB != 0 || srv.balloon.isAutomatic("explicit") {
		t.Fatalf("existing run mutated resources or policy: request=%+v policy=%+v", req, srv.balloon.state("explicit"))
	}
}

func TestBSDDefaultUsesFixedMemoryWithoutBalloon(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 32768, AvailableMB: 32768}})
	req := client.StartInstanceRequest{ID: "bsd", Image: "@freebsd"}
	srv.normalizeStartRequest(&req, nil)
	if req.MemoryMB != defaultBSDGuestMemoryMB || req.BalloonMB != 0 || srv.balloon.isAutomatic("bsd") {
		t.Fatalf("BSD automatic request = %+v policy=%+v", req, srv.balloon.state("bsd"))
	}
}

func TestBSDCreateDefaultUsesFixedMemoryWithoutBalloon(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 32768, AvailableMB: 32768}})
	req := client.CreateInstanceRequest{ID: "bsd-create", Image: "@netbsd"}
	srv.normalizeCreateRequest(&req, nil)
	if req.MemoryMB != defaultBSDGuestMemoryMB || req.BalloonMB != 0 || srv.balloon.isAutomatic("bsd-create") {
		t.Fatalf("BSD create request = %+v policy=%+v", req, srv.balloon.state("bsd-create"))
	}
}

func TestRuntimePressureWaitsForGuestAcknowledgement(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	srv.balloon.setAutomatic("one", true)
	var targets []uint64
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{{ID: "one", Status: "running", MemoryMB: 4096, BalloonStatus: "converged"}},
		balloon:  func(_ string, target uint64) error { targets = append(targets, target); return nil },
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != defaultPolicyStepMB {
		t.Fatalf("first targets = %v", targets)
	}
	runtime.statuses[0].BalloonMB = targets[0]
	runtime.statuses[0].BalloonStatus = "inflating"
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("unacknowledged target advanced again: %v", targets)
	}
	runtime.statuses[0].BalloonActualMB = targets[0]
	runtime.statuses[0].BalloonStatus = "converged"
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[1] != 2*defaultPolicyStepMB {
		t.Fatalf("acknowledged targets = %v", targets)
	}
}

func TestRuntimePolicyReclaimsAndRestoresMemoryFromObservedHostPressure(t *testing.T) {
	memory := fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 256}}
	srv := NewServer("secret")
	srv.balloon = newBalloonController(memory)
	srv.balloon.setAutomatic("one", true)
	srv.balloon.setAutomatic("two", true)
	var changes []balloonDecision
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{
			{ID: "one", Status: "running", MemoryMB: 2048},
			{ID: "two", Status: "running", MemoryMB: 2048},
		},
		balloon: func(id string, target uint64) error {
			changes = append(changes, balloonDecision{ID: id, BalloonMB: target})
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].BalloonMB == 0 || changes[1].BalloonMB == 0 {
		t.Fatalf("pressure changes = %+v", changes)
	}
}

func TestRuntimePressureDoesNotOverrideExplicitBalloonTarget(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 128}})
	req := client.StartInstanceRequest{ID: "explicit", MemoryMB: 2048, BalloonMB: 256}
	srv.normalizeStartRequest(&req, nil)
	called := false
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{{ID: "explicit", Status: "running", MemoryMB: 2048, BalloonMB: 256}},
		balloon:  func(string, uint64) error { called = true; return nil },
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("host pressure policy changed an explicit balloon target")
	}
}

func TestRuntimePressureContinuesAfterIndependentBalloonFailure(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	srv.balloon.setAutomatic("a-failing", true)
	srv.balloon.setAutomatic("b-healthy", true)
	wantErr := errors.New("wedged balloon")
	var healthyTarget uint64
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{
			{ID: "a-failing", Status: "running", MemoryMB: 2048},
			{ID: "b-healthy", Status: "running", MemoryMB: 2048},
		},
		balloon: func(id string, target uint64) error {
			if id == "a-failing" {
				return wantErr
			}
			healthyTarget = target
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); !errors.Is(err, wantErr) {
		t.Fatalf("reconcile error = %v", err)
	}
	if healthyTarget == 0 {
		t.Fatal("healthy VM was not adjusted after another balloon failed")
	}
}

func TestBalloonPolicyPrunesStoppedAndFailedStartEntries(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 4096}})
	for _, id := range []string{"active", "stopped", "failed-start"} {
		srv.balloon.setAutomatic(id, true)
	}
	srv.balloon.mu.Lock()
	failed := srv.balloon.automatic["failed-start"]
	failed.createdAt = time.Now().Add(-2 * time.Minute)
	srv.balloon.automatic["failed-start"] = failed
	srv.balloon.mu.Unlock()
	runtime := fakeRuntimeView{statuses: []client.InstanceState{
		{ID: "active", Status: "running", MemoryMB: 2048},
		{ID: "stopped", Status: "stopped", MemoryMB: 2048},
	}}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	if !srv.balloon.isAutomatic("active") || srv.balloon.isAutomatic("stopped") || srv.balloon.isAutomatic("failed-start") {
		t.Fatalf("policy lifecycle entries = %+v", srv.balloon.automatic)
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
