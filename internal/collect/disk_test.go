package collect

import (
	"testing"

	psutil "github.com/shirou/gopsutil/v3/disk"
)

func TestDiskDeltaLatencyAndBusy(t *testing.T) {
	prev := psutil.IOCountersStat{ReadCount: 100, WriteCount: 50, ReadTime: 1000, WriteTime: 2000, IoTime: 10_000}
	cur := psutil.IOCountersStat{ReadCount: 300, WriteCount: 60, ReadTime: 1400, WriteTime: 2250, IoTime: 11_500}

	d := diskDelta("ada0", cur, prev, 2)
	if d.ReadIOPS != 100 || d.WriteIOPS != 5 {
		t.Errorf("iops: got r=%v w=%v, want 100/5", d.ReadIOPS, d.WriteIOPS)
	}
	// 400 ms over 200 reads, 250 ms over 10 writes, 1500 ms busy of 2000.
	if d.ReadMs != 2 || d.WriteMs != 25 || d.BusyPct != 75 {
		t.Errorf("got read_ms=%v write_ms=%v busy=%v, want 2/25/75", d.ReadMs, d.WriteMs, d.BusyPct)
	}
}

func TestDiskDeltaIdleAndClamp(t *testing.T) {
	prev := psutil.IOCountersStat{ReadCount: 10, ReadTime: 100, IoTime: 0}
	// No completed requests: latency is 0, not NaN. Busy over-reports by a
	// tick boundary and must clamp to 100.
	cur := psutil.IOCountersStat{ReadCount: 10, ReadTime: 100, IoTime: 1010}

	d := diskDelta("ada0", cur, prev, 1)
	if d.ReadMs != 0 || d.WriteMs != 0 {
		t.Errorf("idle latency: got r=%v w=%v, want 0", d.ReadMs, d.WriteMs)
	}
	if d.BusyPct != 100 {
		t.Errorf("busy clamp: got %v, want 100", d.BusyPct)
	}
}

func TestDiskDeltaCounterReset(t *testing.T) {
	prev := psutil.IOCountersStat{ReadCount: 1000, ReadTime: 5000, IoTime: 9000}
	cur := psutil.IOCountersStat{ReadCount: 5, ReadTime: 20, IoTime: 30}

	d := diskDelta("ada0", cur, prev, 1)
	if d.BusyPct != 0 || d.ReadMs != 0 {
		t.Errorf("reset: got busy=%v read_ms=%v, want 0", d.BusyPct, d.ReadMs)
	}
}
