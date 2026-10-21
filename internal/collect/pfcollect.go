package collect

import "time"

// PFCollector turns pfctl's cumulative counters into per-second rates. It is
// platform-neutral; the pfctl calls themselves live in pf.go (FreeBSD) and
// pf_other.go (a stub everywhere else).
//
// pfctl prints its own "Rate" column, but that is a lifetime average since pf
// was enabled, which tells you nothing about what the firewall is doing now —
// so this keeps the previous sample and diffs it like every other rate here.
type PFCollector struct {
	prev   pfRaw
	prevAt time.Time
	limit  uint64 // states hard limit; re-read only when it is still unknown
}

func (c *PFCollector) Collect() *PFStats {
	raw, ok := pfctlInfo()
	if !ok {
		return nil
	}
	now := time.Now()

	// The hard limit is a tunable that effectively never changes, so read it
	// once rather than forking pfctl twice per tick.
	if c.limit == 0 {
		c.limit = pfctlMemoryLimit()
	}

	s := &PFStats{
		Enabled:    raw.enabled,
		States:     raw.states,
		StateLimit: c.limit,
	}
	if c.limit > 0 {
		s.StatePct = round2(float64(raw.states) / float64(c.limit) * 100)
	}
	if !c.prevAt.IsZero() {
		dt := now.Sub(c.prevAt).Seconds()
		if dt <= 0 {
			dt = 1
		}
		s.SearchRate = round2(deltaU64(raw.searches, c.prev.searches) / dt)
		s.InsertRate = round2(deltaU64(raw.inserts, c.prev.inserts) / dt)
		s.MatchRate = round2(deltaU64(raw.matches, c.prev.matches) / dt)
		s.DropRate = round2(deltaU64(raw.drops, c.prev.drops) / dt)
	}
	c.prev, c.prevAt = raw, now
	return s
}
