//go:build freebsd

package collect

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// Temperatures are read from sysctl. The coretemp and amdtemp drivers expose
// dev.cpu.N.temperature as an integer in tenths of degrees Kelvin.
// ACPI thermal zones use hw.acpi.thermal.tzN.temperature in the same format.
func collectTemps() []TempStat {
	var out []TempStat
	out = append(out, readCPUTemps()...)
	out = append(out, readACPITemps()...)
	return out
}

func readCPUTemps() []TempStat {
	var out []TempStat
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("dev.cpu.%d.temperature", i)
		raw, err := unix.SysctlRaw(key)
		if err != nil {
			break
		}
		if c := deciKelvinToCelsius(raw); c != nil {
			out = append(out, TempStat{Name: fmt.Sprintf("cpu%d", i), Celsius: *c})
		}
	}
	return out
}

func readACPITemps() []TempStat {
	var out []TempStat
	for i := 0; i < 16; i++ {
		key := fmt.Sprintf("hw.acpi.thermal.tz%d.temperature", i)
		raw, err := unix.SysctlRaw(key)
		if err != nil {
			break
		}
		if c := deciKelvinToCelsius(raw); c != nil {
			out = append(out, TempStat{Name: fmt.Sprintf("tz%d", i), Celsius: *c})
		}
	}
	return out
}

// deciKelvinToCelsius converts a 4-byte little-endian value (tenths of K) to °C.
func deciKelvinToCelsius(raw []byte) *float64 {
	if len(raw) < 4 {
		return nil
	}
	dk := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
	c := float64(dk)/10.0 - 273.15
	if c < -40 || c > 150 {
		return nil
	}
	c = math.Round(c*10) / 10
	return &c
}
