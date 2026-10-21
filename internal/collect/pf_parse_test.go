package collect

import "testing"

// Real `pfctl -si` output. Note the label column contains spaces ("current
// entries"), which is why rows are matched on their trailing numbers.
const pfctlInfoSample = `Status: Enabled for 3 days 02:11:33           Debug: Urgent

State Table                          Total             Rate
  current entries                      521
  searches                       1174296815        4362.9/s
  inserts                          13725834          51.0/s
  removals                         13725313          51.0/s
Counters
  match                            14628139          54.3/s
  bad-offset                              0           0.0/s
  fragment                               31           0.0/s
  short                                   0           0.0/s
  normalize                               0           0.0/s
  memory                                  7           0.0/s
  bad-timestamp                           0           0.0/s
  congestion                              2           0.0/s
  ip-option                            1210           0.0/s
  proto-cksum                             0           0.0/s
  state-mismatch                        123           0.0/s
  state-insert                            0           0.0/s
  state-limit                             5           0.0/s
  src-limit                               0           0.0/s
  synproxy                                0           0.0/s
`

func TestParsePfctlInfo(t *testing.T) {
	pf := parsePfctlInfo(pfctlInfoSample)
	if !pf.enabled {
		t.Error("want enabled")
	}
	if pf.states != 521 {
		t.Errorf("states = %d, want 521", pf.states)
	}
	if pf.searches != 1174296815 {
		t.Errorf("searches = %d, want 1174296815", pf.searches)
	}
	if pf.inserts != 13725834 {
		t.Errorf("inserts = %d, want 13725834", pf.inserts)
	}
	if pf.matches != 14628139 {
		t.Errorf("matches = %d, want 14628139", pf.matches)
	}
	// memory 7 + congestion 2 + state-limit 5; the other counters mean a
	// packet was malformed, not that pf ran out of room.
	if pf.drops != 14 {
		t.Errorf("drops = %d, want 14 (memory+congestion+state-limit)", pf.drops)
	}
}

func TestParsePfctlInfoDisabled(t *testing.T) {
	pf := parsePfctlInfo("Status: Disabled                              Debug: Urgent\n")
	if pf.enabled {
		t.Error("want disabled")
	}
}

func TestParsePfctlMemory(t *testing.T) {
	out := `states        hard limit    10000
src-nodes     hard limit    10000
frags         hard limit     5000
table-entries hard limit   200000
`
	if got := parsePfctlMemory(out); got != 10000 {
		t.Errorf("states limit = %d, want 10000", got)
	}
	// No states row: report 0 rather than guessing a denominator.
	if got := parsePfctlMemory("frags hard limit 5000\n"); got != 0 {
		t.Errorf("missing states limit should be 0, got %d", got)
	}
}
