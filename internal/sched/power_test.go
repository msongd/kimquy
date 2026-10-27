package sched

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestPowerReasons(t *testing.T) {
	for _, c := range []struct {
		p       Power
		battery bool
		want    []string
	}{
		{Power{}, true, nil},
		{Power{Thermal: ThermalFair}, true, nil},
		{Power{OnBattery: true}, true, []string{PauseBattery}},
		{Power{OnBattery: true}, false, nil},
		{Power{LowPower: true}, false, []string{PauseLowPower}},
		{Power{Thermal: ThermalSerious}, true, []string{PauseThermal}},
		{Power{Thermal: ThermalCritical}, true, []string{PauseThermal}},
		{Power{Thermal: -1}, true, []string{PauseThermal}},
		{Power{OnBattery: true, LowPower: true, Thermal: ThermalCritical}, true, powerReasons},
	} {
		if got := PowerReasons(c.p, c.battery); !slices.Equal(got, c.want) {
			t.Errorf("%+v battery=%v: %v, want %v", c.p, c.battery, got, c.want)
		}
	}
}

func TestPowerMonitor(t *testing.T) {
	q := NewQueue(4)
	var mu sync.Mutex
	state := Power{OnBattery: true}
	set := func(p Power) { mu.Lock(); state = p; mu.Unlock() }
	m := PowerMonitor{
		Probe:          func() (Power, error) { mu.Lock(); defer mu.Unlock(); return state, nil },
		PauseOnBattery: func() bool { return true },
		Every:          5 * time.Millisecond,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	q.Pause("manual")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, q) }()
	wait := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !slices.Equal(q.PausedReasons(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("paused %v, want %v", q.PausedReasons(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait("manual", PauseBattery)
	set(Power{Thermal: ThermalSerious})
	wait("manual", PauseThermal)
	set(Power{})
	wait("manual")
	set(Power{LowPower: true})
	wait(PauseLowPower, "manual")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wait("manual") // its own reasons are cleared on exit
}

func TestPowerMonitorProbeError(t *testing.T) {
	m := PowerMonitor{
		Probe:          func() (Power, error) { return Power{}, io.ErrUnexpectedEOF },
		PauseOnBattery: func() bool { return true },
		Every:          time.Millisecond,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := m.Run(context.Background(), NewQueue(1)); err == nil {
		t.Fatal("probe error ignored")
	}
	if err := (PowerMonitor{}).Run(context.Background(), NewQueue(1)); err == nil {
		t.Fatal("incomplete monitor accepted")
	}
}
