package vmshd

import (
	"errors"
	"strings"
	"sync"
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

func TestBalloonCapacityAccountsForConfiguredMemoryOverhead(t *testing.T) {
	cfg := balloonPolicyConfig{MinUsableMB: 512}
	configured := uint64(20 * 1024)
	wantMinimum := uint64(512 + 20*1024/32)
	capacity := balloonCapacity(balloonVM{ConfiguredMB: configured}, cfg)
	if got := configured - capacity; got != wantMinimum {
		t.Fatalf("minimum usable memory = %d MiB, want %d MiB", got, wantMinimum)
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

func TestAutomaticCommitmentIsNotReleasedFromIdleHostAvailability(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	controller.config = balloonPolicyConfig{ReservePercent: 10, StepMB: 128, HysteresisMB: 256, MinUsableMB: 512}
	target := controller.reserveAutomaticStart("one", 20480, memorySnapshot{TotalMB: 32768, AvailableMB: 10900}, time.Now())
	usable := uint64(20480) - target
	decisions := planBalloonTargetsWithinCommitment(
		memorySnapshot{TotalMB: 32768, AvailableMB: 30000},
		[]balloonVM{{ID: "one", ConfiguredMB: 20480, BalloonMB: target, Eligible: true}}, controller.config, controller.commitmentLimit(),
	)
	if len(decisions) != 1 || decisions[0].BalloonMB != target || controller.commitmentLimit() != usable {
		t.Fatalf("idle commitment target=%d usable=%d decisions=%+v limit=%d", target, usable, decisions, controller.commitmentLimit())
	}
}

func TestAutomaticAdmissionAccountsForExistingAndConcurrentReservations(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	snapshot := memorySnapshot{TotalMB: 32768, AvailableMB: 10900}
	targets := make(chan uint64, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			targets <- controller.reserveAutomaticStart(id, 20480, snapshot, time.Now())
		}(id)
	}
	wg.Wait()
	close(targets)
	var usable uint64
	for target := range targets {
		usable += 20480 - target
	}
	minimum := minimumBalloonUsableMB(20480, controller.config)
	if limit := controller.commitmentLimit(); usable > limit+minimum {
		t.Fatalf("concurrent usable commitment=%d exceeds safety pool=%d plus one minimum guest=%d", usable, limit, minimum)
	}
}

func TestBalloonReleaseIsFairWithinUnusedCommitment(t *testing.T) {
	vms := []balloonVM{
		{ID: "one", ConfiguredMB: 2048, BalloonMB: 1024, Eligible: true},
		{ID: "two", ConfiguredMB: 2048, BalloonMB: 1024, Eligible: true},
	}
	decisions := planBalloonTargetsWithinCommitment(memorySnapshot{TotalMB: 8192, AvailableMB: 8192}, vms, balloonPolicyConfig{}, 4096)
	if decisions[0].BalloonMB != 896 || decisions[1].BalloonMB != 896 {
		t.Fatalf("fair release decisions = %+v", decisions)
	}
}

func TestExplicitDuplicateNormalizationDoesNotChangeAutomaticOwner(t *testing.T) {
	snapshot := memorySnapshot{TotalMB: 8192, AvailableMB: 4096}
	controller := newBalloonController(fakeMemoryObserver{snapshot: snapshot})
	controller.reserveAutomaticStart("shared", 4096, snapshot, time.Now())
	explicit := client.StartInstanceRequest{ID: "shared", Image: "alpine", MemoryMB: 512}
	controller.applyStartRequest(&explicit, nil)
	if !controller.state("shared").Automatic || explicit.MemoryMB != 512 || explicit.BalloonMB != 0 {
		t.Fatalf("duplicate explicit request changed automatic owner: automatic=%+v explicit=%+v", controller.state("shared"), explicit)
	}
}

func TestAutomaticNormalizationCannotClaimRunningExplicitVM(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 4096}})
	req := client.StartInstanceRequest{ID: "shared", Image: "alpine"}
	err := controller.applyStartRequest(&req, []client.InstanceState{{ID: "shared", Status: "running", MemoryMB: 2048}})
	if err == nil {
		t.Fatal("automatic duplicate start was admitted")
	}
	if state := controller.state("shared"); state.Automatic {
		t.Fatalf("failed start claimed explicit VM policy: %+v", state)
	}
}

func TestFailedAutomaticReservationRollsBackExactGeneration(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	token, _, err := controller.reserveStart("replacement", 4096, memorySnapshot{TotalMB: 8192, AvailableMB: 4096}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	controller.completeStart("replacement", token, false)
	if state := controller.state("replacement"); state.Automatic {
		t.Fatalf("failed start retained policy: %+v", state)
	}
	if _, exists := controller.starts["replacement"]; exists {
		t.Fatal("failed start retained admission reservation")
	}
}

func TestStaleBalloonCompletionCannotMutateReplacementGeneration(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	controller.setAutomatic("replacement", true)
	old, ok := controller.beginBalloonAdjustment("replacement", 512, time.Now())
	if !ok {
		t.Fatal("old adjustment was not admitted")
	}
	controller.forget("replacement")
	controller.setAutomatic("replacement", true)
	controller.finishBalloonAdjustment("replacement", old, errors.New("late backend failure"))
	state := controller.state("replacement")
	if state.DegradedReason != "" || state.LastFailure != "" {
		t.Fatalf("stale completion mutated replacement: %+v", state)
	}
}

func TestAutomaticNormalizationReportsMemoryObservationFailure(t *testing.T) {
	if !supportsAutomaticBalloon("alpine") {
		t.Skip("automatic balloon admission is not supported on this platform")
	}
	want := errors.New("memory telemetry unavailable")
	controller := newBalloonController(fakeMemoryObserver{err: want})
	req := client.StartInstanceRequest{ID: "automatic", Image: "alpine"}
	if err := controller.applyStartRequest(&req, nil); !errors.Is(err, want) {
		t.Fatalf("normalization error = %v, want %v", err, want)
	}
	if req.MemoryMB != 0 || req.PolicyToken != 0 {
		t.Fatalf("failed normalization silently chose fixed resources: %+v", req)
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

func TestInitialAutomaticBalloonPreservesObservedHostHeadroom(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	target := controller.initialBalloonTarget(memorySnapshot{TotalMB: 32768, AvailableMB: 10900}, 20480)
	if target != 12856 {
		t.Fatalf("initial balloon target = %d, want 12856 MiB to preserve the host reserve", target)
	}
}

func TestAutomaticPolicyReconcilesInitialTargetWithLiveDevice(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	controller.setAutomatic("new", true)
	controller.markInitialBalloonRequest("new", 1024, time.Now().Add(-time.Minute))
	state := client.InstanceState{ID: "new", Status: "running", MemoryMB: 4096, BalloonStatus: "converged"}
	controller.reconcileLifecycle([]client.InstanceState{state}, time.Now())
	if !controller.adjustmentReady(state, time.Now()) {
		t.Fatal("converged live device remained blocked by the pre-boot target")
	}
	policy := controller.state("new")
	if policy.InFlight || policy.DegradedReason != "" {
		t.Fatalf("policy after live reconciliation = %+v", policy)
	}
}

func TestAutomaticPolicyRecoversAfterLateConvergence(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	controller.config.Convergence = time.Second
	controller.setAutomatic("slow", true)
	controller.markBalloonRequest("slow", 512, time.Now().Add(-2*time.Second))
	inflight := client.InstanceState{ID: "slow", Status: "running", MemoryMB: 4096, BalloonMB: 512, BalloonActualMB: 128, BalloonStatus: "inflating"}
	if controller.adjustmentReady(inflight, time.Now()) {
		t.Fatal("unacknowledged target was reported ready")
	}
	if controller.state("slow").DegradedReason == "" {
		t.Fatal("convergence failure was not exposed")
	}
	converged := inflight
	converged.BalloonActualMB = converged.BalloonMB
	converged.BalloonStatus = "converged"
	if !controller.adjustmentReady(converged, time.Now()) {
		t.Fatal("late convergence did not recover the policy")
	}
	policy := controller.state("slow")
	if policy.DegradedReason != "" || policy.LastFailure == "" {
		t.Fatalf("recovered policy did not retain failure history: %+v", policy)
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
	srv.balloon.commitmentLimitMB = 4096
	var mu sync.Mutex
	var targets []uint64
	adjusted := make(chan struct{}, 2)
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{{ID: "one", Status: "running", MemoryMB: 4096, BalloonStatus: "converged"}},
		balloon: func(_ string, target uint64) error {
			mu.Lock()
			targets = append(targets, target)
			mu.Unlock()
			adjusted <- struct{}{}
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adjusted:
	case <-time.After(10 * time.Second):
		t.Fatal("first balloon adjustment did not complete")
	}
	mu.Lock()
	if len(targets) != 1 || targets[0] != 819 {
		t.Fatalf("first targets = %v", targets)
	}
	firstTarget := targets[0]
	mu.Unlock()
	runtime.statuses[0].BalloonMB = firstTarget
	runtime.statuses[0].BalloonStatus = "inflating"
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(targets) != 1 {
		t.Fatalf("unacknowledged target advanced again: %v", targets)
	}
	mu.Unlock()
	runtime.statuses[0].BalloonActualMB = firstTarget
	runtime.statuses[0].BalloonStatus = "converged"
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adjusted:
	case <-time.After(10 * time.Second):
		t.Fatal("second balloon adjustment did not complete")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 2 || targets[1] != 1638 {
		t.Fatalf("acknowledged targets = %v", targets)
	}
}

func TestRuntimePolicyReclaimsAndRestoresMemoryFromObservedHostPressure(t *testing.T) {
	memory := fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 256}}
	srv := NewServer("secret")
	srv.balloon = newBalloonController(memory)
	srv.balloon.setAutomatic("one", true)
	srv.balloon.setAutomatic("two", true)
	srv.balloon.commitmentLimitMB = 4096
	var mu sync.Mutex
	var changes []balloonDecision
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{
			{ID: "one", Status: "running", MemoryMB: 2048},
			{ID: "two", Status: "running", MemoryMB: 2048},
		},
		balloon: func(id string, target uint64) error {
			mu.Lock()
			defer mu.Unlock()
			changes = append(changes, balloonDecision{ID: id, BalloonMB: target})
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	requireEventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(changes) == 2 })
	mu.Lock()
	defer mu.Unlock()
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
	srv.balloon.commitmentLimitMB = 4096
	wantErr := errors.New("wedged balloon")
	var healthyMu sync.Mutex
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
			healthyMu.Lock()
			healthyTarget = target
			healthyMu.Unlock()
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatalf("reconcile error = %v", err)
	}
	requireEventually(t, func() bool {
		healthyMu.Lock()
		defer healthyMu.Unlock()
		return healthyTarget != 0 && srv.balloon.state("a-failing").DegradedReason != ""
	})
	healthyMu.Lock()
	defer healthyMu.Unlock()
	if healthyTarget == 0 || !strings.Contains(srv.balloon.state("a-failing").DegradedReason, wantErr.Error()) {
		t.Fatal("healthy VM was not adjusted after another balloon failed")
	}
}

func TestRuntimePressureDoesNotQueueBehindStuckBalloonDevice(t *testing.T) {
	srv := NewServer("secret")
	srv.balloon = newBalloonController(fakeMemoryObserver{snapshot: memorySnapshot{TotalMB: 8192, AvailableMB: 0}})
	srv.balloon.setAutomatic("a-stuck", true)
	srv.balloon.setAutomatic("b-healthy", true)
	srv.balloon.commitmentLimitMB = 4096
	release := make(chan struct{})
	healthy := make(chan struct{}, 1)
	runtime := fakeRuntimeView{
		statuses: []client.InstanceState{
			{ID: "a-stuck", Status: "running", MemoryMB: 2048},
			{ID: "b-healthy", Status: "running", MemoryMB: 2048},
		},
		balloon: func(id string, _ uint64) error {
			if id == "a-stuck" {
				<-release
				return nil
			}
			healthy <- struct{}{}
			return nil
		},
	}
	if err := srv.reconcileBalloonPressure(runtime); err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthy:
	case <-time.After(time.Second):
		t.Fatal("healthy VM adjustment queued behind a stuck balloon device")
	}
	close(release)
}

func TestBalloonPolicyKeepsBackendTombstonesAndPrunesFailedStarts(t *testing.T) {
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
	if !srv.balloon.isAutomatic("active") || !srv.balloon.isAutomatic("stopped") || srv.balloon.isAutomatic("failed-start") {
		t.Fatalf("policy lifecycle entries = %+v", srv.balloon.automatic)
	}
}

func TestAutomaticPolicySurvivesBackendTombstoneUntilMCPReap(t *testing.T) {
	controller := newBalloonController(fakeMemoryObserver{})
	controller.setAutomatic("automatic", true)
	running := client.InstanceState{
		ID: "automatic", Status: "running", MemoryMB: 4096,
		BalloonMB: 768, BalloonActualMB: 768, BalloonStatus: "converged",
	}
	controller.reconcileLifecycle([]client.InstanceState{running}, time.Now())
	if !controller.adjustmentReady(running, time.Now()) {
		t.Fatal("live automatic VM was not ready")
	}
	controller.reconcileLifecycle([]client.InstanceState{{ID: "automatic", Status: "stopped", MemoryMB: 4096, BalloonMB: 128}}, time.Now())
	state := controller.state("automatic")
	if !state.Automatic || state.TargetMB != 768 || state.ActualMB != 768 || state.Status != "converged" {
		t.Fatalf("tombstone policy = %+v", state)
	}
	controller.forget("automatic")
	if controller.state("automatic").Automatic {
		t.Fatal("explicit MCP reap retained automatic policy state")
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
