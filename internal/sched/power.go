package sched

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// Thermal is the system's thermal state, as NSProcessInfo reports it.
type Thermal int

const (
	ThermalNominal Thermal = iota
	ThermalFair
	ThermalSerious
	ThermalCritical
)

func (t Thermal) String() string {
	switch t {
	case ThermalNominal:
		return "nominal"
	case ThermalFair:
		return "fair"
	case ThermalSerious:
		return "serious"
	case ThermalCritical:
		return "critical"
	}
	return fmt.Sprintf("thermal(%d)", int(t))
}

// Power is a snapshot of the conditions that pause indexing.
type Power struct {
	OnBattery bool
	LowPower  bool // Low Power Mode
	Thermal   Thermal
}

// Pause reasons set by the power policy.
const (
	PauseBattery  = "on battery"
	PauseLowPower = "low power mode"
	PauseThermal  = "thermal state serious or worse"
)

var powerReasons = []string{PauseBattery, PauseLowPower, PauseThermal}

// PowerReasons decides which pause reasons apply. An unknown thermal state
// counts as hot.
func PowerReasons(p Power, pauseOnBattery bool) []string {
	var r []string
	if p.OnBattery && pauseOnBattery {
		r = append(r, PauseBattery)
	}
	if p.LowPower {
		r = append(r, PauseLowPower)
	}
	if p.Thermal >= ThermalSerious || p.Thermal < ThermalNominal {
		r = append(r, PauseThermal)
	}
	return r
}

// PowerMonitor polls Probe and pauses q for as long as a reason applies.
type PowerMonitor struct {
	Probe          func() (Power, error)
	PauseOnBattery func() bool // read each poll, so config changes apply
	Every          time.Duration
	Log            *slog.Logger
}

// Run applies the policy until ctx ends; a probe error is returned, since
// indexing would otherwise go on regardless of power.
func (m PowerMonitor) Run(ctx context.Context, q *Queue) error {
	if m.Probe == nil || m.PauseOnBattery == nil || m.Every <= 0 || m.Log == nil {
		return fmt.Errorf("sched: incomplete power monitor %+v", m)
	}
	active := map[string]bool{}
	defer func() { // leave no stale pause behind
		for r := range active {
			q.Resume(r)
		}
	}()
	t := time.NewTicker(m.Every)
	defer t.Stop()
	for {
		p, err := m.Probe()
		if err != nil {
			return fmt.Errorf("power state: %w", err)
		}
		want := PowerReasons(p, m.PauseOnBattery())
		for _, r := range want {
			if !active[r] {
				m.Log.Info("pausing indexing", "reason", r)
				q.Pause(r)
				active[r] = true
			}
		}
		for _, r := range slices.Sorted(maps.Keys(active)) {
			if !slices.Contains(want, r) {
				m.Log.Info("resuming indexing", "reason", r)
				q.Resume(r)
				delete(active, r)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
