package vmshd

import (
	"context"
	"errors"
	"log/slog"
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
)

type memorySnapshot struct {
	TotalMB     uint64
	AvailableMB uint64
}

type memoryObserver interface {
	Snapshot() (memorySnapshot, error)
}

type balloonController struct {
	memory    memoryObserver
	config    balloonPolicyConfig
	mu        sync.Mutex
	automatic map[string]balloonPolicyEntry
}

type balloonPolicyEntry struct {
	automatic bool
	createdAt time.Time
	seen      bool
}

type balloonPolicyConfig struct {
	ReservePercent uint64
	StepMB         uint64
	HysteresisMB   uint64
	MinUsableMB    uint64
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
	c.setAutomatic(req.ID, automatic)
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
	req.BalloonMB = c.initialBalloonTarget(snapshot, current, req.ID, req.MemoryMB)
}

func (c *balloonController) applyCreateRequest(req *client.CreateInstanceRequest, current []client.InstanceState) {
	if req == nil {
		return
	}
	start := client.StartInstanceRequest{
		ID:        req.ID,
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
	automatic := req.MemoryMB == 0 && req.BalloonMB == 0
	c.setAutomatic(req.ID, automatic)
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
	req.BalloonMB = c.initialBalloonTarget(snapshot, current, req.ID, req.MemoryMB)
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
	c.automatic[id] = balloonPolicyEntry{automatic: automatic, createdAt: time.Now()}
	c.mu.Unlock()
}

func (c *balloonController) isAutomatic(id string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	automatic := c.automatic[strings.TrimSpace(id)].automatic
	c.mu.Unlock()
	return automatic
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
		if ok && state.Status == "running" {
			entry.seen = true
			c.automatic[id] = entry
			continue
		}
		// A policy entry which reached the runtime can be removed as soon as
		// the VM disappears. Entries for failed starts receive a short grace
		// period so the monitor cannot race request normalization and startup.
		if ok || entry.seen || now.Sub(entry.createdAt) >= time.Minute {
			delete(c.automatic, id)
		}
	}
}

func (c *balloonController) initialBalloonTarget(snapshot memorySnapshot, current []client.InstanceState, id string, memoryMB uint64) uint64 {
	projected := snapshot
	if projected.AvailableMB > memoryMB {
		projected.AvailableMB -= memoryMB
	} else {
		projected.AvailableMB = 0
	}
	vms := balloonVMsFromInstances(current)
	id = strings.TrimSpace(id)
	if id == "" {
		id = "__new__"
	}
	vms = append(vms, balloonVM{ID: id, ConfiguredMB: memoryMB, Eligible: true})
	decisions := planBalloonTargets(projected, vms, c.config)
	for _, decision := range decisions {
		if decision.ID == id {
			return decision.BalloonMB
		}
	}
	return 0
}

func balloonVMsFromInstances(states []client.InstanceState) []balloonVM {
	out := make([]balloonVM, 0, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.ID) == "" || state.MemoryMB == 0 || state.Status != "running" {
			continue
		}
		out = append(out, balloonVM{ID: state.ID, ConfiguredMB: state.MemoryMB, BalloonMB: state.BalloonMB, Eligible: true})
	}
	return out
}

func planBalloonTargets(snapshot memorySnapshot, vms []balloonVM, cfg balloonPolicyConfig) []balloonDecision {
	cfg = normalizeBalloonPolicyConfig(cfg)
	decisions := make([]balloonDecision, len(vms))
	for i, vm := range vms {
		decisions[i] = balloonDecision{ID: vm.ID, BalloonMB: min(vm.BalloonMB, balloonCapacity(vm, cfg))}
	}
	targetAvailable := snapshot.TotalMB * cfg.ReservePercent / 100
	if snapshot.AvailableMB < targetAvailable {
		distributeBalloonIncrease(decisions, vms, cfg, targetAvailable-snapshot.AvailableMB)
		return decisions
	}
	if snapshot.AvailableMB > targetAvailable+cfg.HysteresisMB {
		distributeBalloonDecrease(decisions, cfg, snapshot.AvailableMB-targetAvailable-cfg.HysteresisMB)
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
	return cfg
}

func distributeBalloonIncrease(decisions []balloonDecision, vms []balloonVM, cfg balloonPolicyConfig, needMB uint64) {
	for needMB > 0 {
		progress := false
		for i, vm := range vms {
			if needMB == 0 {
				return
			}
			capacity := balloonCapacity(vm, cfg)
			if !vm.Eligible || decisions[i].BalloonMB >= capacity {
				continue
			}
			delta := min(cfg.StepMB, needMB, capacity-decisions[i].BalloonMB)
			decisions[i].BalloonMB += delta
			needMB -= delta
			progress = true
		}
		if !progress {
			return
		}
	}
}

func distributeBalloonDecrease(decisions []balloonDecision, cfg balloonPolicyConfig, releaseMB uint64) {
	for releaseMB > 0 {
		progress := false
		for i := range decisions {
			if releaseMB == 0 {
				return
			}
			if decisions[i].BalloonMB == 0 {
				continue
			}
			delta := min(cfg.StepMB, releaseMB, decisions[i].BalloonMB)
			decisions[i].BalloonMB -= delta
			releaseMB -= delta
			progress = true
		}
		if !progress {
			return
		}
	}
}

func balloonCapacity(vm balloonVM, cfg balloonPolicyConfig) uint64 {
	if vm.ConfiguredMB <= cfg.MinUsableMB {
		return 0
	}
	return vm.ConfiguredMB - cfg.MinUsableMB
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
	s.balloon.reconcileLifecycle(states, time.Now())
	vms := balloonVMsFromInstances(states)
	vms = slices.DeleteFunc(vms, func(vm balloonVM) bool { return !s.balloon.isAutomatic(vm.ID) })
	decisions := planBalloonTargets(snapshot, vms, s.balloon.config)
	current := make(map[string]uint64, len(vms))
	for _, vm := range vms {
		current[vm.ID] = vm.BalloonMB
	}
	var errs []error
	for _, decision := range decisions {
		if current[decision.ID] == decision.BalloonMB {
			continue
		}
		if err := runtime.SetInstanceBalloon(decision.ID, decision.BalloonMB); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
