package power

import (
	"runtime"
	"testing"

	"github.com/msongd/kimquy/internal/sched"
)

func TestProbe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs macOS")
	}
	p, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if p.Thermal < sched.ThermalNominal || p.Thermal > sched.ThermalCritical {
		t.Fatalf("thermal state %v out of range", p.Thermal)
	}
	t.Logf("%+v (thermal %v)", p, p.Thermal)
}
