//go:build !darwin

// Package power reads the battery, Low Power Mode and thermal state that
// pause indexing (see sched.PowerReasons).
package power

import (
	"errors"

	"github.com/msongd/kimquy/internal/sched"
)

// Probe is only implemented on macOS.
func Probe() (sched.Power, error) {
	return sched.Power{}, errors.New("power: state is only readable on macOS")
}
