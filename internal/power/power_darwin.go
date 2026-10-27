// Package power reads the battery, Low Power Mode and thermal state that
// pause indexing (see sched.PowerReasons).
package power

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation -framework Foundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/ps/IOPowerSources.h>
#include <IOKit/ps/IOPSKeys.h>
#import <Foundation/Foundation.h>

// semdOnBattery: 1 on battery, 0 on AC (or no battery), -1 unknown.
static int semdOnBattery(void) {
	CFTypeRef info = IOPSCopyPowerSourcesInfo();
	if (info == NULL) return -1;
	CFStringRef src = IOPSGetProvidingPowerSourceType(info); // not owned
	int r = -1;
	if (src != NULL) {
		r = CFStringCompare(src, CFSTR(kIOPMBatteryPowerKey), 0) == kCFCompareEqualTo ? 1 : 0;
	}
	CFRelease(info);
	return r;
}

static int semdLowPower(void) {
	@autoreleasepool {
		return [[NSProcessInfo processInfo] isLowPowerModeEnabled] ? 1 : 0;
	}
}

static long semdThermal(void) {
	@autoreleasepool {
		return (long)[[NSProcessInfo processInfo] thermalState];
	}
}
*/
import "C"

import (
	"errors"

	"github.com/msongd/kimquy/internal/sched"
)

// Probe returns the current power conditions.
func Probe() (sched.Power, error) {
	var p sched.Power
	switch C.semdOnBattery() {
	case 1:
		p.OnBattery = true
	case 0:
	default:
		return p, errors.New("cannot read the power source")
	}
	p.LowPower = C.semdLowPower() == 1
	// NSProcessInfoThermalState: nominal 0, fair 1, serious 2, critical 3.
	p.Thermal = sched.Thermal(C.semdThermal())
	return p, nil
}
