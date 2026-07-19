package vmshd

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
)

const (
	defaultReservePercent       = 10
	defaultPolicyStepMB         = 128
	defaultPolicyHysteresisMB   = 256
	defaultMinimumGuestUsableMB = 512
	defaultFixedGuestMemoryMB   = 512
	defaultBSDGuestMemoryMB     = 1024
	defaultBalloonConvergence   = 15 * time.Second
)

type memorySnapshot struct {
	TotalMB     uint64
	AvailableMB uint64
}

type memoryObserver interface {
	Snapshot() (memorySnapshot, error)
}

type balloonController struct {
	memory            memoryObserver
	config            balloonPolicyConfig
	mu                sync.Mutex
	automatic         map[string]balloonPolicyEntry
	commitmentLimitMB uint64
}

type balloonPolicyEntry struct {
	automatic            bool
	createdAt            time.Time
	seen                 bool
	inFlight             bool
	requestedMB          uint64
	requestedAt          time.Time
	lastActualMB         uint64
	lastTargetMB         uint64
	lastObservedTargetMB uint64
	lastStatus           string
	initialRequest       bool
	configuredMB         uint64
	committedMB          uint64
	active               bool
	adjusting            bool
	degradedReason       string
	lastFailure          string
}

type balloonPolicyState struct {
	Automatic        bool
	InFlight         bool
	DegradedReason   string
	LastFailure      string
	TargetMB         uint64
	ActualMB         uint64
	Status           string
	ObservedTargetMB uint64
}

type balloonPolicyConfig struct {
	ReservePercent uint64
	StepMB         uint64
	HysteresisMB   uint64
	MinUsableMB    uint64
	Convergence    time.Duration
}

type balloonVM struct {
	ID           string
	ConfiguredMB uint64
	BalloonMB    uint64
	Eligible     bool
}

type balloonDecision struct {
	ID        string
	BalloonMB uint64
}

type balloonRuntime interface {
	InstanceStatuses() []client.InstanceState
	SetInstanceBalloon(string, uint64) error
}

func newBalloonController(memory memoryObserver) *balloonController {
	return &balloonController{
		memory: memory,
		config: balloonPolicyConfig{
			ReservePercent: defaultReservePercent,
			StepMB:         defaultPolicyStepMB,
			HysteresisMB:   defaultPolicyHysteresisMB,
			MinUsableMB:    defaultMinimumGuestUsableMB,
		},
		automatic: make(map[string]balloonPolicyEntry),
	}
}

func defaultGuestMemoryMB(hostMemoryMB uint64) uint64 {
	if hostMemoryMB == 0 {
		return 0
	}
	if hostMemoryMB <= 4096 {
		return min(hostMemoryMB, 2048)
	}
	target := hostMemoryMB * 3 / 4
	rounded := target / 4096 * 4096
	if rounded == 0 {
		return min(hostMemoryMB, 2048)
	}
	return rounded
}

func (c *balloonController) applyStartRequest(req *client.StartInstanceRequest, current []client.InstanceState) {
	if c == nil || c.memory == nil || req == nil {
		return
	}
	automatic := req.MemoryMB == 0 && req.BalloonMB == 0
	if automatic && !supportsAutomaticBalloon(req.Image) {
		req.MemoryMB = fixedGuestMemoryMB(req.Image)
		return
	}
	if !automatic {
		return
	}
	snapshot, err := c.memory.Snapshot()
	if err != nil || snapshot.TotalMB == 0 {
		return
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = defaultGuestMemoryMB(snapshot.TotalMB)
	}
	if req.MemoryMB == 0 {
		return
	}
	req.BalloonMB = c.reserveAutomaticStart(req.ID, req.MemoryMB, snapshot, time.Now())
}

func (c *balloonController) applyCreateRequest(req *client.CreateInstanceRequest, current []client.InstanceState) {
	if req == nil {
		return
	}
	start := client.StartInstanceRequest{
		ID:        req.ID,
		Image:     req.Image,
		MemoryMB:  req.MemoryMB,
		BalloonMB: req.BalloonMB,
	}
	c.applyStartRequest(&start, current)
	req.MemoryMB = start.MemoryMB
	req.BalloonMB = start.BalloonMB
}

func (c *balloonController) applyRunRequest(req *client.RunRequest, current []client.InstanceState) {
	if c == nil || c.memory == nil || req == nil {
		return
	}
	// A run request without an image executes in an existing instance. Its
	// zero-valued resource fields mean "leave the instance alone", not
	// "re-enrol this VM in automatic memory management".
	if strings.TrimSpace(req.Image) == "" {
		return
	}
	start := client.StartInstanceRequest{ID: req.ID, Image: req.Image, MemoryMB: req.MemoryMB, BalloonMB: req.BalloonMB}
	c.applyStartRequest(&start, current)
	req.MemoryMB, req.BalloonMB = start.MemoryMB, start.BalloonMB
}

func supportsAutomaticBalloon(image string) bool {
	if isMCPBuiltinBSDImage(image) {
		return false
	}
	return runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

func fixedGuestMemoryMB(image string) uint64 {
	if isMCPBuiltinBSDImage(image) {
		return defaultBSDGuestMemoryMB
	}
	return defaultFixedGuestMemoryMB
}

func (c *balloonController) setAutomatic(id string, automatic bool) {
	if c == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	c.mu.Lock()
	c.automatic[id] = balloonPolicyEntry{automatic: automatic, createdAt: time.Now(), active: automatic}
	c.mu.Unlock()
}

func (c *balloonController) reserveAutomaticStart(id string, memoryMB uint64, snapshot memorySnapshot, now time.Time) uint64 {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	cfg := normalizeBalloonPolicyConfig(c.config)
	reserve := snapshot.TotalMB * cfg.ReservePercent / 100
	var safelyBackable uint64
	if snapshot.AvailableMB > reserve {
		safelyBackable = snapshot.AvailableMB - reserve
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.automatic[id]; ok && existing.automatic {
		return existing.lastTargetMB
	}
	if !c.hasActiveAutomaticLocked() {
		c.commitmentLimitMB = safelyBackable
	}
	committed := c.activeCommitmentLocked()
	available := uint64(0)
	if c.commitmentLimitMB > committed {
		available = c.commitmentLimitMB - committed
	}
	usable := min(memoryMB, available)
	target := min(memoryMB-usable, balloonCapacity(balloonVM{ConfiguredMB: memoryMB}, cfg))
	usable = memoryMB - target
	c.automatic[id] = balloonPolicyEntry{
		automatic: true, createdAt: now, active: true, configuredMB: memoryMB, committedMB: usable,
		inFlight: target != 0, requestedMB: target, requestedAt: now, lastTargetMB: target, initialRequest: target != 0,
	}
	return target
}

func (c *balloonController) hasActiveAutomaticLocked() bool {
	for _, entry := range c.automatic {
		if entry.automatic && entry.active {
			return true
		}
	}
	return false
}

func (c *balloonController) activeCommitmentLocked() uint64 {
	var committed uint64
	for _, entry := range c.automatic {
		if entry.automatic && entry.active {
			committed += entry.committedMB
		}
	}
	return committed
}

func (c *balloonController) commitmentLimit() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commitmentLimitMB
}

func (c *balloonController) isAutomatic(id string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	entry := c.automatic[strings.TrimSpace(id)]
	automatic := entry.automatic && entry.degradedReason == ""
	c.mu.Unlock()
	return automatic
}

func (c *balloonController) state(id string) balloonPolicyState {
	if c == nil {
		return balloonPolicyState{}
	}
	c.mu.Lock()
	entry := c.automatic[strings.TrimSpace(id)]
	c.mu.Unlock()
	return balloonPolicyState{
		Automatic: entry.automatic, InFlight: entry.inFlight, DegradedReason: entry.degradedReason, LastFailure: entry.lastFailure,
		TargetMB: entry.lastTargetMB, ActualMB: entry.lastActualMB, Status: entry.lastStatus,
		ObservedTargetMB: entry.lastObservedTargetMB,
	}
}

func (c *balloonController) markInitialBalloonRequest(id string, targetMB uint64, now time.Time) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	c.markBalloonRequest(id, targetMB, now)
	c.mu.Lock()
	entry := c.automatic[id]
	entry.initialRequest = true
	c.automatic[id] = entry
	c.mu.Unlock()
}

func (c *balloonController) markBalloonRequest(id string, targetMB uint64, now time.Time) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	c.mu.Lock()
	entry := c.automatic[id]
	entry.inFlight = true
	entry.requestedMB = targetMB
	entry.lastTargetMB = targetMB
	entry.requestedAt = now
	c.automatic[id] = entry
	c.mu.Unlock()
}

func (c *balloonController) adjustmentReady(state client.InstanceState, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.automatic[strings.TrimSpace(state.ID)]
	if !ok || !entry.automatic {
		return false
	}
	if state.BalloonStatus == "unsupported" {
		entry.degradedReason = "dynamic ballooning is unsupported by this VM"
		c.automatic[state.ID] = entry
		return false
	}
	actual := state.BalloonActualMB
	if state.BalloonStatus == "" {
		actual = state.BalloonMB
	}
	entry.lastObservedTargetMB = state.BalloonMB
	if !entry.inFlight {
		entry.lastTargetMB = state.BalloonMB
	}
	entry.lastActualMB = actual
	entry.lastStatus = state.BalloonStatus
	if entry.degradedReason != "" {
		if state.BalloonStatus == "driver_unavailable" || state.BalloonMB != actual {
			return false
		}
		entry.lastFailure = entry.degradedReason
		entry.degradedReason = ""
		entry.inFlight = false
	}
	if !entry.inFlight && state.BalloonMB != actual {
		entry.inFlight = true
		entry.requestedMB = state.BalloonMB
		entry.requestedAt = now
	}
	if entry.inFlight {
		entry.lastActualMB = actual
		if actual == entry.requestedMB && state.BalloonStatus != "driver_unavailable" {
			entry.inFlight = false
			c.automatic[state.ID] = entry
			return true
		} else if now.Sub(entry.requestedAt) >= normalizeBalloonPolicyConfig(c.config).Convergence {
			entry.degradedReason = "guest did not acknowledge the balloon target before the convergence deadline"
		}
		c.automatic[state.ID] = entry
		return false
	}
	if entry.adjusting {
		c.automatic[state.ID] = entry
		return false
	}
	entry.lastActualMB = actual
	c.automatic[state.ID] = entry
	return state.BalloonStatus != "driver_unavailable"
}

func (c *balloonController) markBalloonFailure(id string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	entry := c.automatic[strings.TrimSpace(id)]
	entry.degradedReason = err.Error()
	entry.lastFailure = err.Error()
	entry.inFlight = false
	entry.adjusting = false
	c.automatic[strings.TrimSpace(id)] = entry
	c.mu.Unlock()
}

func (c *balloonController) beginBalloonAdjustment(id string, targetMB uint64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.automatic[strings.TrimSpace(id)]
	if !ok || !entry.automatic || !entry.active || entry.adjusting {
		return false
	}
	entry.adjusting = true
	entry.inFlight = true
	entry.requestedMB = targetMB
	entry.lastTargetMB = targetMB
	entry.requestedAt = now
	c.automatic[strings.TrimSpace(id)] = entry
	return true
}

func (c *balloonController) finishBalloonAdjustment(id string, err error) {
	if err != nil {
		c.markBalloonFailure(id, err)
		return
	}
	c.mu.Lock()
	entry := c.automatic[strings.TrimSpace(id)]
	entry.adjusting = false
	c.automatic[strings.TrimSpace(id)] = entry
	c.mu.Unlock()
}

func (c *balloonController) reconcileLifecycle(states []client.InstanceState, now time.Time) {
	if c == nil {
		return
	}
	present := make(map[string]client.InstanceState, len(states))
	for _, state := range states {
		present[strings.TrimSpace(state.ID)] = state
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, entry := range c.automatic {
		state, ok := present[id]
		if ok && (state.Status == "running" || state.Status == "starting" || state.Status == "stopping") {
			entry.active = true
			entry.configuredMB = state.MemoryMB
			actual := state.BalloonActualMB
			if state.BalloonStatus == "" {
				actual = state.BalloonMB
			}
			entry.lastObservedTargetMB = state.BalloonMB
			if !entry.inFlight {
				entry.lastTargetMB = state.BalloonMB
			}
			entry.lastActualMB = actual
			entry.lastStatus = state.BalloonStatus
			if state.Status == "running" && !entry.seen {
				entry.seen = true
				if entry.inFlight && entry.initialRequest {
					// Start requests are normalized before the backend creates the
					// balloon device. The live device state is authoritative once the
					// guest is running: a restored or freshly initialized device may
					// legitimately begin at a different target.
					entry.requestedMB = state.BalloonMB
					entry.initialRequest = false
					actual := state.BalloonActualMB
					if state.BalloonStatus == "" {
						actual = state.BalloonMB
					}
					if state.BalloonStatus != "driver_unavailable" && actual == state.BalloonMB {
						entry.inFlight = false
					}
				}
				if entry.inFlight {
					entry.requestedAt = now
				}
			}
			c.automatic[id] = entry
			continue
		}
		// Preserve policy identity and the last live device state alongside a
		// stopped/crashed backend tombstone. MCP reaping explicitly forgets the
		// policy. Only requests which never reached a runtime are aged out here.
		if entry.seen {
			entry.active = false
			c.automatic[id] = entry
			continue
		}
		if ok {
			// A guest may shut down before the monitor samples its brief running
			// state. A backend tombstone still proves the request reached the
			// runtime, so retain its automatic identity until MCP reaps it.
			entry.active = false
			c.automatic[id] = entry
			continue
		}
		if now.Sub(entry.createdAt) >= time.Minute {
			delete(c.automatic, id)
		}
	}
}

func (c *balloonController) forget(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.automatic, strings.TrimSpace(id))
	if !c.hasActiveAutomaticLocked() {
		c.commitmentLimitMB = 0
	}
	c.mu.Unlock()
}

func (c *balloonController) initialBalloonTarget(snapshot memorySnapshot, memoryMB uint64) uint64 {
	cfg := normalizeBalloonPolicyConfig(c.config)
	reserve := snapshot.TotalMB * cfg.ReservePercent / 100
	var safelyBackable uint64
	if snapshot.AvailableMB > reserve {
		safelyBackable = snapshot.AvailableMB - reserve
	}
	// Automatic memory is a dynamic ceiling, not a promise that the host can
	// immediately back every configured guest page. Start with enough balloon
	// to preserve observed host headroom even if the guest dirties memory in a
	// tight loop before the reactive monitor gets another scheduling turn.
	needed := memoryMB - min(memoryMB, safelyBackable)
	return min(needed, balloonCapacity(balloonVM{ConfiguredMB: memoryMB}, cfg))
}

func balloonVMsFromInstances(states []client.InstanceState) []balloonVM {
	out := make([]balloonVM, 0, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.ID) == "" || state.MemoryMB == 0 || state.Status != "running" {
			continue
		}
		actual := state.BalloonActualMB
		if state.BalloonStatus == "" {
			actual = state.BalloonMB
		}
		out = append(out, balloonVM{ID: state.ID, ConfiguredMB: state.MemoryMB, BalloonMB: actual, Eligible: state.BalloonStatus != "unsupported"})
	}
	return out
}

func planBalloonTargets(snapshot memorySnapshot, vms []balloonVM, cfg balloonPolicyConfig) []balloonDecision {
	return planBalloonTargetsWithinCommitment(snapshot, vms, cfg, ^uint64(0))
}

func planBalloonTargetsWithinCommitment(snapshot memorySnapshot, vms []balloonVM, cfg balloonPolicyConfig, commitmentLimitMB uint64) []balloonDecision {
	cfg = normalizeBalloonPolicyConfig(cfg)
	decisions := make([]balloonDecision, len(vms))
	for i, vm := range vms {
		decisions[i] = balloonDecision{ID: vm.ID, BalloonMB: min(vm.BalloonMB, balloonCapacity(vm, cfg))}
	}
	var committedUsable uint64
	for i, vm := range vms {
		committedUsable += vm.ConfiguredMB - decisions[i].BalloonMB
	}
	targetAvailable := snapshot.TotalMB * cfg.ReservePercent / 100
	pressureNeed := uint64(0)
	if snapshot.AvailableMB < targetAvailable {
		pressureNeed = targetAvailable - snapshot.AvailableMB
	}
	commitmentNeed := uint64(0)
	if committedUsable > commitmentLimitMB {
		commitmentNeed = committedUsable - commitmentLimitMB
	}
	if need := max(pressureNeed, commitmentNeed); need != 0 {
		distributeBalloonIncrease(decisions, vms, cfg, need)
		return decisions
	}
	if snapshot.AvailableMB > targetAvailable+cfg.HysteresisMB {
		unusedCommitment := uint64(0)
		if commitmentLimitMB > committedUsable {
			unusedCommitment = commitmentLimitMB - committedUsable
		}
		release := min(unusedCommitment, snapshot.AvailableMB-targetAvailable-cfg.HysteresisMB)
		distributeBalloonDecrease(decisions, vms, cfg, release)
	}
	return decisions
}

func normalizeBalloonPolicyConfig(cfg balloonPolicyConfig) balloonPolicyConfig {
	if cfg.ReservePercent == 0 {
		cfg.ReservePercent = defaultReservePercent
	}
	if cfg.StepMB == 0 {
		cfg.StepMB = defaultPolicyStepMB
	}
	if cfg.HysteresisMB == 0 {
		cfg.HysteresisMB = defaultPolicyHysteresisMB
	}
	if cfg.MinUsableMB == 0 {
		cfg.MinUsableMB = defaultMinimumGuestUsableMB
	}
	if cfg.Convergence <= 0 {
		cfg.Convergence = defaultBalloonConvergence
	}
	return cfg
}

func distributeBalloonIncrease(decisions []balloonDecision, vms []balloonVM, cfg balloonPolicyConfig, needMB uint64) {
	for needMB > 0 {
		eligible := 0
		for i, vm := range vms {
			if vm.Eligible && decisions[i].BalloonMB < balloonCapacity(vm, cfg) {
				eligible++
			}
		}
		if eligible == 0 {
			return
		}
		share := (needMB + uint64(eligible) - 1) / uint64(eligible)
		progress := uint64(0)
		for i, vm := range vms {
			capacity := balloonCapacity(vm, cfg)
			if !vm.Eligible || decisions[i].BalloonMB >= capacity {
				continue
			}
			delta := min(share, needMB, capacity-decisions[i].BalloonMB)
			decisions[i].BalloonMB += delta
			needMB -= delta
			progress += delta
		}
		if progress == 0 {
			return
		}
	}
}

func distributeBalloonDecrease(decisions []balloonDecision, vms []balloonVM, cfg balloonPolicyConfig, releaseMB uint64) {
	eligible := 0
	for i, vm := range vms {
		if vm.Eligible && decisions[i].BalloonMB != 0 {
			eligible++
		}
	}
	releaseMB = min(releaseMB, cfg.StepMB*uint64(eligible))
	for releaseMB > 0 && eligible > 0 {
		share := (releaseMB + uint64(eligible) - 1) / uint64(eligible)
		progress := uint64(0)
		eligible = 0
		for i, vm := range vms {
			if !vm.Eligible || decisions[i].BalloonMB == 0 {
				continue
			}
			delta := min(share, releaseMB, decisions[i].BalloonMB)
			decisions[i].BalloonMB -= delta
			releaseMB -= delta
			progress += delta
			if decisions[i].BalloonMB != 0 {
				eligible++
			}
		}
		if progress == 0 {
			return
		}
	}
}

func balloonCapacity(vm balloonVM, cfg balloonPolicyConfig) uint64 {
	minimum := minimumBalloonUsableMB(vm.ConfiguredMB, cfg)
	if vm.ConfiguredMB <= minimum {
		return 0
	}
	return vm.ConfiguredMB - minimum
}

func minimumBalloonUsableMB(configuredMB uint64, cfg balloonPolicyConfig) uint64 {
	cfg = normalizeBalloonPolicyConfig(cfg)
	// A balloon target counts memory that Linux cannot make available to
	// userspace: struct page metadata and other boot-time allocations grow with
	// the configured physical address space. Keeping only a fixed 512 MiB from a
	// 20 GiB guest therefore leaves roughly 100 MiB allocatable and can panic the
	// kernel while the guest agent starts. Account for that scaling overhead in
	// addition to the actual working floor. This is a minimum availability
	// guarantee, not a guest resource cap; the controller can still deflate the
	// balloon up to the configured memory when durable host backing is available.
	minimum := cfg.MinUsableMB + configuredMB/32
	return min(configuredMB, minimum)
}

func (s *Server) normalizeStartRequest(req *client.StartInstanceRequest, runtime ccvmd.RuntimeView) {
	if s == nil || s.balloon == nil {
		return
	}
	s.balloon.applyStartRequest(req, runtimeInstanceStatuses(runtime))
}

func (s *Server) normalizeCreateRequest(req *client.CreateInstanceRequest, runtime ccvmd.RuntimeView) {
	if s == nil || s.balloon == nil {
		return
	}
	s.balloon.applyCreateRequest(req, runtimeInstanceStatuses(runtime))
}

func (s *Server) normalizeRunRequest(req *client.RunRequest, runtime ccvmd.RuntimeView) {
	if s == nil || s.balloon == nil {
		return
	}
	s.balloon.applyRunRequest(req, runtimeInstanceStatuses(runtime))
}

func (s *Server) startBalloonMonitor(runtime balloonRuntime) {
	if s == nil || s.balloon == nil || runtime == nil {
		return
	}
	s.balloonMonitorOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.balloonMonitorCancel = cancel
		go s.monitorBalloonPressure(ctx, runtime)
	})
}

func (s *Server) stopBalloonMonitor() {
	if s != nil && s.balloonMonitorCancel != nil {
		s.balloonMonitorCancel()
	}
}

func (s *Server) monitorBalloonPressure(ctx context.Context, runtime balloonRuntime) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := s.reconcileBalloonPressure(runtime); err != nil {
			slog.Warn("dynamic VM memory recovery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) reconcileBalloonPressure(runtime balloonRuntime) error {
	if s == nil || s.balloon == nil || s.balloon.memory == nil || runtime == nil {
		return nil
	}
	snapshot, err := s.balloon.memory.Snapshot()
	if err != nil || snapshot.TotalMB == 0 {
		return err
	}
	states := runtime.InstanceStatuses()
	now := time.Now()
	s.balloon.reconcileLifecycle(states, now)
	vms := balloonVMsFromInstances(states)
	stateByID := make(map[string]client.InstanceState, len(states))
	for _, state := range states {
		stateByID[state.ID] = state
	}
	vms = slices.DeleteFunc(vms, func(vm balloonVM) bool {
		return !s.balloon.adjustmentReady(stateByID[vm.ID], now)
	})
	decisions := planBalloonTargetsWithinCommitment(snapshot, vms, s.balloon.config, s.balloon.commitmentLimit())
	current := make(map[string]uint64, len(vms))
	for _, vm := range vms {
		current[vm.ID] = vm.BalloonMB
	}
	for _, decision := range decisions {
		if current[decision.ID] == decision.BalloonMB {
			continue
		}
		if !s.balloon.beginBalloonAdjustment(decision.ID, decision.BalloonMB, now) {
			continue
		}
		go func(id string, targetMB uint64) {
			s.balloon.finishBalloonAdjustment(id, runtime.SetInstanceBalloon(id, targetMB))
		}(decision.ID, decision.BalloonMB)
	}
	return nil
}

func min[T ~uint64](values ...T) T {
	if len(values) == 0 {
		return 0
	}
	out := values[0]
	for _, value := range values[1:] {
		if value < out {
			out = value
		}
	}
	return out
}
