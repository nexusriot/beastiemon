package collect

import (
	"context"
	"sync"
	"time"
)

// hostHealthTimeout bounds one check. pkg audit normally takes well under a
// second, but a locked pkg database must not wedge the refresh loop forever.
const hostHealthTimeout = time.Minute

// HostHealthCollector runs the host-health check on its own interval in the
// background and hands the sampler the latest result. The checks shell out
// to pkg/bectl and change on the scale of days, so running them every tick
// would cost far more than they are worth.
type HostHealthCollector struct {
	every time.Duration
	check func(context.Context) *HostHealth

	mu    sync.Mutex
	last  *HostHealth
	ready chan struct{}
	once  sync.Once
}

func NewHostHealthCollector(every time.Duration, vulnXML string) *HostHealthCollector {
	return &HostHealthCollector{
		every: every,
		check: func(ctx context.Context) *HostHealth { return checkHostHealth(ctx, vulnXML) },
		ready: make(chan struct{}),
	}
}

// Run refreshes immediately, then every interval, until ctx is done.
func (h *HostHealthCollector) Run(ctx context.Context) {
	for {
		cctx, cancel := context.WithTimeout(ctx, hostHealthTimeout)
		r := h.check(cctx)
		cancel()
		h.mu.Lock()
		h.last = r
		h.mu.Unlock()
		h.once.Do(func() { close(h.ready) })

		select {
		case <-ctx.Done():
			return
		case <-time.After(h.every):
		}
	}
}

// Ready is closed once the first check has finished.
func (h *HostHealthCollector) Ready() <-chan struct{} { return h.ready }

// Latest returns the most recent result, or nil before the first one.
func (h *HostHealthCollector) Latest() *HostHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}
