package outboundgroup

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

// FailoverOption describes how a group picks the node of a new connection.
//
// A group without failover dials the single node it currently prefers, so the
// caller gets that node's error even when other nodes of the group are usable.
type FailoverOption struct {
	// Race dials up to N healthy nodes concurrently and keeps the first one that
	// answers, the remaining attempts are cancelled. 0 or 1 disables racing.
	Race int
	// Retry dials the next healthy node when the preferred one failed, up to N
	// extra attempts. Ignored while Race is enabled.
	Retry int
	// WarmStandbys keeps N healthy nodes besides the node in use pre-connected,
	// so that a failover does not have to establish a fresh session first.
	WarmStandbys int
	// WarmInterval is the delay between two warm rounds in seconds, 0 reuses the
	// group interval.
	WarmInterval int
}

type failoverResult struct {
	index int
	proxy C.Proxy
	conn  C.Conn
	err   error
}

type failoverDialer struct {
	group           *GroupBase
	current         func() C.Proxy
	pinned          func(C.Proxy) bool
	testUrl         string
	healthCheck     func()
	expectedStatus  utils.IntRanges[uint16]
	race            int
	retry           int
	warmStandbys    int
	warmInterval    time.Duration
	testTimeout     time.Duration
	warmMutex       sync.Mutex
	warmNextWarmRun time.Time
}

func newFailoverDialer(group *GroupBase, current func() C.Proxy, pinned func(C.Proxy) bool, testUrl, expectedStatus string, option FailoverOption, interval int) *failoverDialer {
	status, err := utils.NewUnsignedRanges[uint16](expectedStatus)
	if err != nil {
		log.Warnln("[Group](%s) invalid expected-status %q: %v", group.Name(), expectedStatus, err)
		status = nil // an empty range accepts any status
	}

	d := &failoverDialer{
		group:          group,
		current:        current,
		pinned:         pinned,
		testUrl:        testUrl,
		healthCheck:    group.healthCheck,
		expectedStatus: status,
		race:           option.Race,
		retry:          option.Retry,
		warmStandbys:   option.WarmStandbys,
		warmInterval:   time.Duration(option.WarmInterval) * time.Second,
		testTimeout:    time.Duration(group.testTimeout) * time.Millisecond,
	}
	if d.race < 2 {
		d.race = 0
	}
	if d.retry < 0 {
		d.retry = 0
	}
	if d.warmStandbys < 0 {
		d.warmStandbys = 0
	}
	if d.warmInterval <= 0 {
		d.warmInterval = time.Duration(interval) * time.Second
	}
	if d.warmInterval <= 0 {
		d.warmInterval = 5 * time.Minute
	}

	if d.race > 0 || d.retry > 0 || d.warmStandbys > 0 {
		log.Infoln(
			"[Group](%s) failover enabled: race=%d retry=%d warm-standbys=%d warm-interval=%s",
			group.Name(), d.race, d.retry, d.warmStandbys, d.warmInterval,
		)
	}

	return d
}

// dial returns a connection through one of the healthy nodes of the group and
// reports which node finally served it.
func (d *failoverDialer) dial(ctx context.Context, metadata *C.Metadata) (C.Proxy, C.Conn, error) {
	primary := d.current()
	candidates := d.candidates(primary)
	d.warmStandbysAsync(candidates)

	// an explicitly pinned node is an operator decision, racing other nodes
	// would silently overrule it; sequential retry still applies
	if d.race < 2 || len(candidates) < 2 || d.pinned(primary) {
		return d.sequentialDial(ctx, metadata, candidates)
	}
	return d.raceDial(ctx, metadata, candidates)
}

// sequentialDial dials the preferred node and falls back to the next healthy
// nodes while attempts are left.
func (d *failoverDialer) sequentialDial(ctx context.Context, metadata *C.Metadata, candidates []C.Proxy) (C.Proxy, C.Conn, error) {
	attempts := 1 + d.retry
	proxy := candidates[0]
	var lastErr error
	for i, candidate := range candidates {
		if i >= attempts {
			break
		}
		if i > 0 && ctx.Err() != nil {
			break
		}
		conn, err := d.dialOne(ctx, candidate, metadata)
		if err == nil {
			return candidate, conn, nil
		}
		proxy, lastErr = candidate, err
	}
	return proxy, nil, lastErr
}

// raceDial dials the first healthy nodes at the same time and keeps the fastest
// one, which hides both broken nodes that fail late and nodes that never answer.
func (d *failoverDialer) raceDial(ctx context.Context, metadata *C.Metadata, candidates []C.Proxy) (C.Proxy, C.Conn, error) {
	count := d.race
	if count > len(candidates) {
		count = len(candidates)
	}

	attemptCtx := make([]context.Context, count)
	attemptCancel := make([]context.CancelFunc, count)
	for i := 0; i < count; i++ {
		attemptCtx[i], attemptCancel[i] = context.WithCancel(ctx)
	}

	results := make(chan failoverResult, count)
	for i := 0; i < count; i++ {
		go func(i int) {
			conn, err := d.dialOne(attemptCtx[i], candidates[i], metadata)
			results <- failoverResult{index: i, proxy: candidates[i], conn: conn, err: err}
		}(i)
	}

	var (
		proxy   C.Proxy
		lastErr error
	)
	for received := 0; received < count; received++ {
		var result failoverResult
		select {
		case result = <-results:
		case <-ctx.Done():
			for _, cancel := range attemptCancel {
				cancel()
			}
			// every attempt that was not consumed yet is still in flight
			go d.drainResults(ctx, results, count-received)
			return proxy, nil, ctx.Err()
		}

		lastErr = result.err
		if result.err == nil {
			proxy = result.proxy
			// keep the context of the winner alive: only the losing attempts are
			// cancelled, their connections are closed as soon as they arrive
			for i := 0; i < count; i++ {
				if i != result.index {
					attemptCancel[i]()
				}
			}
			go d.drainResults(ctx, results, count-received-1)
			return proxy, result.conn, nil
		}

		proxy = result.proxy
		attemptCancel[result.index]()
	}
	return proxy, nil, lastErr
}

// drainResults consumes the attempts that lost the race and releases whatever
// they returned in the meantime.
func (d *failoverDialer) drainResults(ctx context.Context, results <-chan failoverResult, remaining int) {
	for i := 0; i < remaining; i++ {
		result := <-results
		if result.conn != nil {
			_ = result.conn.Close()
		}
		if result.err != nil {
			d.reportDialFailed(ctx, result.proxy, result.err)
		}
	}
}

func (d *failoverDialer) dialOne(ctx context.Context, proxy C.Proxy, metadata *C.Metadata) (C.Conn, error) {
	// every attempt works on its own copy: proxies may resolve or rewrite the
	// destination of the metadata they are given
	conn, err := proxy.DialContext(ctx, metadata.Clone())
	if err != nil {
		d.reportDialFailed(ctx, proxy, err)
		return nil, err
	}
	return conn, nil
}

// reportDialFailed feeds the failure back into the group, which demotes the
// node once enough failures piled up.
//
// A cancelled attempt (we won the race) or a dial that only failed because the
// caller ran out of time says nothing about the node, while a deadline raised
// inside the proxy - the OpenVPN handshake timeout for example - does.
func (d *failoverDialer) reportDialFailed(ctx context.Context, proxy C.Proxy, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return
	}
	d.group.onDialFailed(proxy.Type(), err, d.healthCheck)
}

// candidates orders the nodes for a new connection: the node the group wants to
// use first, followed by the remaining healthy nodes ordered by their delay.
//
// While racing, the slots are filled up with nodes that are currently marked
// dead when there are not enough healthy ones: otherwise a pool in which every
// node was marked dead would make each new connection wait for the full dial
// deadline of a single node instead of finding out that one of them recovered.
func (d *failoverDialer) candidates(primary C.Proxy) []C.Proxy {
	proxies := d.group.GetProxies(false)
	candidates := make([]C.Proxy, 0, len(proxies))
	candidates = append(candidates, primary)

	var dead []C.Proxy
	for _, proxy := range proxies {
		if proxy == primary {
			continue
		}
		if !proxy.AliveForTestUrl(d.testUrl) {
			dead = append(dead, proxy)
			continue
		}
		candidates = append(candidates, proxy)
	}

	if len(candidates) > 2 {
		slices.SortStableFunc(candidates[1:], func(a, b C.Proxy) int {
			return cmp.Compare(a.LastDelayForTestUrl(d.testUrl), b.LastDelayForTestUrl(d.testUrl))
		})
	}

	if missing := d.race - len(candidates); missing > 0 && len(dead) > 0 {
		if missing > len(dead) {
			missing = len(dead)
		}
		candidates = append(candidates, dead[:missing]...)
	}

	return candidates
}

// warmStandbysAsync keeps the standby nodes pre-connected while the group is in
// use. Rounds are rate limited so that a busy group does not probe the standbys
// for every connection it serves.
func (d *failoverDialer) warmStandbysAsync(candidates []C.Proxy) {
	if d.warmStandbys <= 0 || len(candidates) < 2 {
		return
	}

	now := time.Now()
	d.warmMutex.Lock()
	defer d.warmMutex.Unlock()
	if now.Before(d.warmNextWarmRun) {
		return
	}
	d.warmNextWarmRun = now.Add(d.warmInterval)

	// the candidates are a snapshot and the probes must not delay the connection
	// that triggered them
	go d.warmStandbysRound(candidates)
}

func (d *failoverDialer) warmStandbysRound(candidates []C.Proxy) {
	count := d.warmStandbys + 1 // node in use plus its standbys
	if count > len(candidates) {
		count = len(candidates)
	}

	var waitGroup sync.WaitGroup
	for _, proxy := range candidates[:count] {
		proxy := proxy
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			ctx, cancel := context.WithTimeout(context.Background(), d.testTimeout)
			defer cancel()
			delay, err := proxy.URLTest(ctx, d.testUrl, d.expectedStatus)
			if err != nil {
				log.Debugln("[Group](%s) warm standby %s is not ready: %v", d.group.Name(), proxy.Name(), err)
				return
			}
			log.Debugln("[Group](%s) warm standby %s ready, delay: %d ms", d.group.Name(), proxy.Name(), delay)
		}()
	}
	waitGroup.Wait()
}
