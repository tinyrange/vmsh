package progress

import (
	"fmt"
	"time"
)

type Snapshot struct {
	Operation string
	Unit      string
	Done      int64
	Total     int64
	Started   time.Time
	Updated   time.Time
}

func (s Snapshot) Message(now time.Time) string {
	if s.Operation == "" {
		s.Operation = "working"
	}
	if s.Started.IsZero() {
		s.Started = now
	}
	elapsed := now.Sub(s.Started).Round(time.Second)
	if elapsed < time.Second {
		elapsed = 0
	}
	unit := s.Unit
	if unit == "" {
		unit = "units"
	}
	if s.Total > 0 {
		pct := float64(s.Done) * 100 / float64(s.Total)
		if pct > 100 {
			pct = 100
		}
		return fmt.Sprintf("%s: %d/%d %s (%.0f%%, %s)", s.Operation, s.Done, s.Total, unit, pct, elapsed)
	}
	if s.Done > 0 {
		return fmt.Sprintf("%s: %d %s (%s)", s.Operation, s.Done, unit, elapsed)
	}
	return fmt.Sprintf("%s (%s)", s.Operation, elapsed)
}

type Reporter struct {
	ch chan Snapshot
}

func NewReporter() *Reporter {
	return &Reporter{ch: make(chan Snapshot, 8)}
}

func (r *Reporter) Update(s Snapshot) {
	if r == nil {
		return
	}
	if s.Updated.IsZero() {
		s.Updated = time.Now()
	}
	select {
	case r.ch <- s:
	default:
		select {
		case <-r.ch:
		default:
		}
		r.ch <- s
	}
}

func (r *Reporter) C() <-chan Snapshot {
	if r == nil {
		return nil
	}
	return r.ch
}
