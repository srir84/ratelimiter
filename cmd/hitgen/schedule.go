package main

// The precomputed open-loop schedule (DESIGN §B4 "Schedule", "Arrivals and users", "Routing";
// IMPLEMENTATION_PLAN H2 steps 2 and 3): every planned arrival, its user, request ID and
// target instance, built from the scenario and its seed alone and written to schedule.jsonl
// before anything is sent (TRF-1). Nothing here reads a clock, a global random source or a Go
// map's iteration order, so the same scenario and seed give the same bytes on the same
// architecture. Across architectures they may not: math.Pow, which makes the Zipf weights, can
// differ in the last bit between arm64 and amd64, and a weight one unit off can change every
// later user draw. Schedules that are compared are built on one machine.

import (
	"bufio"
	"cmp"
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"

	"ratelimiter/harness/config"
)

// The entry kinds in schedule.jsonl (and gen.jsonl, whose retries add "retry").
const (
	kindInitial   = "initial"   // a new operation: its own request ID
	kindDuplicate = "duplicate" // repeats an earlier initial entry's user and request ID (TRF-6)
)

// The PCG stream of each schedule source (plan choice): each source draws from its own
// rand.New(rand.NewPCG(seed, stream)), so what one source draws never shifts another's. Every
// stream here is below 1<<32; the retry jitter streams (1<<32 | i<<2 | attempt) start there.
const (
	streamAggregate  uint64 = 1       // the steps' arrival gaps, then the user of each arrival
	streamMarkov     uint64 = 2       // every user's quiet/active/burst dwell times and moves
	streamDuplicates uint64 = 3       // which aggregate arrivals repeat an earlier entry, and which
	streamRowBase    uint64 = 1 << 16 // [[streams]] row r draws from streamRowBase + r
)

// totalWeight is what the users' integer draw weights add up to at most: the Zipf and state
// weights are scaled onto integers, so the Fenwick tree's sums are exact and a user whose
// weight is 0 (quiet, or outside its cohort window) can never be drawn.
const totalWeight = 1 << 62

// Entry is one planned arrival (schedule.jsonl): entry I, sent PlannedNs after the run starts
// to instance Instance, for step Step (0-based), as User with RequestID.
type Entry struct {
	I         int
	PlannedNs int64
	Step      int
	User      string
	RequestID string
	Instance  int    // I mod instances + 1 (TRF-4)
	Kind      string // kindInitial or kindDuplicate
	// DuplicateOf is the I of the earlier initial entry a duplicate repeats; -1 for an
	// initial entry.
	DuplicateOf int
}

// BuildSchedule builds the scenario's schedule (§B4; plan H2 step 2): each step is a Poisson
// process at its rate, restarting at the step's start; each of its arrivals goes to a user
// drawn with weight 1/rank^zipf_s × state weight among the users that are active or bursting
// and inside their cohort window, and is left out, counted in skippedNoEligible, when no user
// is; every [[streams]] user adds its own Poisson arrivals over the whole run, outside its
// cohort's gaps (plan choice); the arrivals of all sources merge by planned time (ties: the
// steps first, then the [[streams]] rows in order, a row's users in order) and are numbered
// i = 0…, with request ID r%09d from i and instance i mod instances + 1. With [duplicates], an
// aggregate arrival instead repeats, with probability fraction, an earlier initial entry chosen
// uniformly among those whose user is eligible at the arrival's planned time (inside its
// cohort window and, with [users.states], active or bursting); with none, it stays an initial
// entry. sc is a scenario LoadScenario accepted; for one that fails Validate there is no
// schedule (nil, 0).
func BuildSchedule(sc config.Scenario) (entries []Entry, skippedNoEligible int) {
	if sc.Validate() != nil {
		return nil, 0
	}
	b := newBuilder(sc)
	arrivals, skipped := b.aggregate()
	arrivals = b.streams(arrivals)
	slices.SortFunc(arrivals, plannedOrder)
	return b.number(arrivals), skipped
}

// arrival is one planned arrival before numbering: its time, user index and source. seq is
// unique and grows in source order, so sorting by (ns, seq) is total.
type arrival struct {
	ns        int64
	user      int
	seq       int
	aggregate bool // from the steps (it may become a duplicate)
}

// plannedOrder orders the merged arrivals by planned time, ties in source order (seq): the
// steps' arrivals first, then the [[streams]] rows in order, a row's users in order.
func plannedOrder(x, y arrival) int {
	if c := cmp.Compare(x.ns, y.ns); c != 0 {
		return c
	}
	return cmp.Compare(x.seq, y.seq)
}

// builder holds what every source needs: the step boundaries and the cohort windows.
type builder struct {
	sc       config.Scenario
	bounds   []int64 // bounds[k] is step k's start; bounds[len(steps)] the run's end, ns
	cohortOf []int   // each user's cohort index, -1 for none
	windows  []window
	ids      []string // user IDs, made on first use
}

// window is a cohort's active time: [start, stop) and again from revisit, in ns; stop and
// revisit are math.MaxInt64 when the cohort never stops or never returns.
type window struct {
	start, stop, revisit int64
}

func (w window) active(ns int64) bool {
	return (ns >= w.start && ns < w.stop) || ns >= w.revisit
}

func newBuilder(sc config.Scenario) *builder {
	b := &builder{sc: sc, bounds: stepBounds(sc), cohortOf: make([]int, sc.Users.Count), ids: make([]string, sc.Users.Count)}
	for u := range b.cohortOf {
		b.cohortOf[u] = -1
	}
	for c, co := range sc.Cohorts {
		w := window{start: config.SecondsToNs(co.StartS), stop: math.MaxInt64, revisit: math.MaxInt64}
		if co.StopS != nil {
			w.stop = config.SecondsToNs(*co.StopS)
		}
		if co.RevisitS != nil {
			w.revisit = config.SecondsToNs(*co.RevisitS)
		}
		b.windows = append(b.windows, w)
		for u := co.Users.First; u <= co.Users.Last; u++ {
			b.cohortOf[u] = c
		}
	}
	return b
}

// stepBounds returns the steps' bounds in ns from the run's start: step k is
// [bounds[k], bounds[k+1]), each duration rounded to whole ns.
func stepBounds(sc config.Scenario) []int64 {
	bounds := make([]int64, len(sc.Steps)+1)
	for k, st := range sc.Steps {
		bounds[k+1] = bounds[k] + config.SecondsToNs(st.DurationS)
	}
	return bounds
}

// runNs is the run's length.
func (b *builder) runNs() int64 { return b.bounds[len(b.bounds)-1] }

// inWindow reports whether user u is inside its cohort window at ns (always, outside cohorts).
func (b *builder) inWindow(u int, ns int64) bool {
	c := b.cohortOf[u]
	return c < 0 || b.windows[c].active(ns)
}

// stepOf returns the index of the step whose [start, end) holds ns.
func (b *builder) stepOf(ns int64) int {
	k, _ := slices.BinarySearch(b.bounds, ns+1) // the first bound after ns
	return k - 1
}

// userID is u's ID, u%06d (plan choice).
func (b *builder) userID(u int) string {
	if b.ids[u] == "" {
		b.ids[u] = fmt.Sprintf("u%06d", u)
	}
	return b.ids[u]
}

// poisson appends to times the arrivals of a Poisson process at rate per second over
// [start, end) ns, drawn from rng: exponential gaps from start.
func poisson(rng *rand.Rand, rate float64, start, end int64, times []int64) []int64 {
	if rate <= 0 {
		return times
	}
	span := float64(end - start)
	// The conversions round each gap before it is added, so a fused multiply-add cannot change
	// a time. The schedule as a whole is byte-identical only on one architecture (see the file
	// comment).
	t := 0.0
	for {
		t += float64(rng.ExpFloat64() / rate * 1e9)
		if t >= span {
			return times
		}
		times = append(times, start+int64(t))
	}
}

// aggregate plans the steps' arrivals: first every step's arrival times, then, in time order,
// each arrival's user, drawn among the eligible users at that time with the same generator, so
// the times never depend on who was eligible. An arrival with no eligible user is left out and
// counted.
func (b *builder) aggregate() (out []arrival, skipped int) {
	rng := rand.New(rand.NewPCG(b.sc.Seed, streamAggregate))
	var times []int64
	for k, st := range b.sc.Steps {
		times = poisson(rng, st.RatePerS, b.bounds[k], b.bounds[k+1], times)
	}
	if len(times) == 0 {
		return nil, 0
	}
	p := newPicker(b.sc)
	events := b.cohortEvents()
	for _, u := range b.initiallyOut() {
		p.setInWindow(u, false)
	}
	next := 0
	for _, t := range times {
		for ; next < len(events) && events[next].ns <= t; next++ {
			ev := events[next]
			co := b.sc.Cohorts[ev.cohort]
			for u := co.Users.First; u <= co.Users.Last; u++ {
				p.setInWindow(u, ev.in)
			}
		}
		p.advance(t)
		if p.total == 0 {
			skipped++
			continue
		}
		out = append(out, arrival{ns: t, user: p.find(rng.Int64N(p.total)), seq: len(out), aggregate: true})
	}
	return out, skipped
}

// cohortEvent is a cohort window's edge: its users enter (in) or leave it at ns.
type cohortEvent struct {
	ns     int64
	cohort int
	in     bool
}

// cohortEvents lists every cohort's window edges after time 0, in time order (ties: cohort
// order, then start, stop, revisit).
func (b *builder) cohortEvents() []cohortEvent {
	var evs []cohortEvent
	for c, w := range b.windows {
		if w.start > 0 {
			evs = append(evs, cohortEvent{ns: w.start, cohort: c, in: true})
		}
		if w.stop != math.MaxInt64 {
			evs = append(evs, cohortEvent{ns: w.stop, cohort: c, in: false})
		}
		if w.revisit != math.MaxInt64 {
			evs = append(evs, cohortEvent{ns: w.revisit, cohort: c, in: true})
		}
	}
	slices.SortStableFunc(evs, func(x, y cohortEvent) int { return cmp.Compare(x.ns, y.ns) })
	return evs
}

// initiallyOut lists the users whose cohort window has not started at time 0.
func (b *builder) initiallyOut() []int {
	var out []int
	for c, w := range b.windows {
		if w.start > 0 {
			co := b.sc.Cohorts[c]
			for u := co.Users.First; u <= co.Users.Last; u++ {
				out = append(out, u)
			}
		}
	}
	return out
}

// streams appends every [[streams]] user's arrivals over the whole run, each row from its own
// generator, the row's users in order; an arrival outside the user's cohort window is not
// planned (the stream pauses with its cohort).
func (b *builder) streams(out []arrival) []arrival {
	stream := streamRowBase
	for _, row := range b.sc.Streams {
		rng := rand.New(rand.NewPCG(b.sc.Seed, stream))
		stream++
		for u := row.Users.First; u <= row.Users.Last; u++ {
			for _, t := range poisson(rng, row.RatePerS, 0, b.runNs(), nil) {
				if b.inWindow(u, t) {
					out = append(out, arrival{ns: t, user: u, seq: len(out)})
				}
			}
		}
	}
	return out
}

// number turns the merged arrivals into entries: i, step, instance (TRF-4), and either a new
// request ID or, for a duplicate (TRF-6), the user and request ID of an earlier initial entry
// whose user is eligible at the duplicate's planned time (dupPool).
func (b *builder) number(arrivals []arrival) []Entry {
	var dup *rand.Rand
	var pool *dupPool
	if b.sc.Duplicates.Fraction > 0 {
		dup = rand.New(rand.NewPCG(b.sc.Seed, streamDuplicates))
		pool = b.newDupPool()
	}
	entries := make([]Entry, len(arrivals))
	for i, a := range arrivals {
		e := Entry{I: i, PlannedNs: a.ns, Step: b.stepOf(a.ns), Instance: i%b.sc.Instances + 1,
			Kind: kindInitial, DuplicateOf: -1}
		if pool != nil {
			pool.advance(a.ns)
		}
		if dup != nil && a.aggregate && dup.Float64() < b.sc.Duplicates.Fraction && pool.total > 0 {
			of := pool.pick(dup)
			e.User, e.RequestID = entries[of].User, entries[of].RequestID
			e.Kind, e.DuplicateOf = kindDuplicate, of
		} else {
			e.User, e.RequestID = b.userID(a.user), fmt.Sprintf("r%09d", i)
			if pool != nil {
				pool.add(a.user, i)
			}
		}
		entries[i] = e
	}
	return entries
}

// dupPool holds the earlier initial entries a duplicate may repeat: those whose user is
// eligible at the duplicate's planned time (§B4 cohorts and states, like the user draws),
// inside its cohort window and, with [users.states], active or bursting. It replays the cohort
// edges and the users' Markov moves as time advances (the moves come from their own stream, so
// they are the ones the user draws saw), and a Fenwick tree holds each eligible user's count of
// initial entries, so a uniform choice among the eligible entries is O(log n).
type dupPool struct {
	b       *builder
	tree    []int64 // Fenwick tree over the users, 1-based
	top     int     // the largest power of two ≤ the user count
	total   int64   // the eligible initial entries so far
	entries [][]int // each user's initial entries (their i), in order
	inWin   []bool
	state   []userState
	moves   *markov // nil without [users.states]: every user always active
	events  []cohortEvent
	next    int // the first cohort event not yet applied
}

func (b *builder) newDupPool() *dupPool {
	n := b.sc.Users.Count
	d := &dupPool{b: b, tree: make([]int64, n+1), top: 1, entries: make([][]int, n), inWin: make([]bool, n),
		state: make([]userState, n), events: b.cohortEvents()}
	for d.top*2 <= n {
		d.top *= 2
	}
	for u := range n {
		d.inWin[u], d.state[u] = true, active
	}
	for _, u := range b.initiallyOut() {
		d.inWin[u] = false
	}
	if st := b.sc.Users.States; st != nil {
		d.moves = newMarkov(b.sc.Seed, *st, n)
	}
	return d
}

// eligible reports whether user u is inside its cohort window and active or bursting now.
func (d *dupPool) eligible(u int) bool { return d.inWin[u] && d.state[u] != quiet }

// advance applies every cohort edge and state move at or before ns.
func (d *dupPool) advance(ns int64) {
	for ; d.next < len(d.events) && d.events[d.next].ns <= ns; d.next++ {
		ev := d.events[d.next]
		co := d.b.sc.Cohorts[ev.cohort]
		for u := co.Users.First; u <= co.Users.Last; u++ {
			d.set(u, ev.in, d.state[u])
		}
	}
	for d.moves != nil && d.moves.next() <= ns {
		u, s := d.moves.move()
		d.set(u, d.inWin[u], s)
	}
}

// set gives user u its window flag and state, moving its initial entries into or out of the
// tree when its eligibility changes.
func (d *dupPool) set(u int, in bool, s userState) {
	was := d.eligible(u)
	d.inWin[u], d.state[u] = in, s
	if now := d.eligible(u); now != was {
		n := int64(len(d.entries[u]))
		if !now {
			n = -n
		}
		d.addCount(u, n)
	}
}

// add records initial entry i, of user u.
func (d *dupPool) add(u, i int) {
	d.entries[u] = append(d.entries[u], i)
	if d.eligible(u) {
		d.addCount(u, 1)
	}
}

// addCount adds delta to user u's count in the tree.
func (d *dupPool) addCount(u int, delta int64) {
	d.total += delta
	for i := u + 1; i < len(d.tree); i += i & -i {
		d.tree[i] += delta
	}
}

// pick returns the i of an eligible initial entry chosen uniformly (total > 0): one draw in
// [0, total) names a user's share of the tree and the entry within it.
func (d *dupPool) pick(rng *rand.Rand) int {
	target := rng.Int64N(d.total)
	pos := 0
	for step := d.top; step > 0; step >>= 1 {
		if j := pos + step; j < len(d.tree) && d.tree[j] <= target {
			pos = j
			target -= d.tree[j]
		}
	}
	return d.entries[pos][target]
}

// userState is a user's Markov state (plan choice: quiet, active, burst).
type userState uint8

const (
	quiet userState = iota
	active
	burst
)

// picker draws the user of each aggregate arrival: a Fenwick tree over the users' current
// integer weights (plan choice), O(log n) per change and per draw, and the users' Markov
// states, which move as time advances.
type picker struct {
	tree    []int64 // Fenwick tree, 1-based
	top     int     // the largest power of two ≤ the user count
	weight  []int64 // each user's weight in the tree now
	total   int64
	wActive []int64 // each user's weight while active, and while bursting
	wBurst  []int64
	state   []userState
	inWin   []bool
	moves   *markov // nil without [users.states]: every user always active
}

func newPicker(sc config.Scenario) *picker {
	n := sc.Users.Count
	p := &picker{tree: make([]int64, n+1), top: 1, weight: make([]int64, n), state: make([]userState, n),
		inWin: make([]bool, n)}
	for p.top*2 <= n {
		p.top *= 2
	}
	activeW, burstW := 1.0, 1.0
	if st := sc.Users.States; st != nil {
		activeW, burstW = st.Active.Weight, st.Burst.Weight
		p.moves = newMarkov(sc.Seed, *st, n)
	}
	p.wActive, p.wBurst = zipfWeights(n, sc.Users.ZipfS, activeW, burstW)
	for u := range n {
		p.state[u], p.inWin[u] = active, true
		p.weight[u] = p.wActive[u]
		p.tree[u+1] = p.weight[u]
		p.total += p.weight[u]
	}
	for i := 1; i <= n; i++ { // the linear Fenwick build
		if j := i + i&-i; j <= n {
			p.tree[j] += p.tree[i]
		}
	}
	return p
}

// zipfWeights returns each user's integer draw weight while active and while bursting: user u
// (rank u+1) gets 1/(u+1)^s × the state's weight, scaled so the largest possible total (every
// user in the heavier state) is totalWeight; an eligible user's weight is at least 1, so it
// can be drawn.
func zipfWeights(n int, s, activeW, burstW float64) (wActive, wBurst []int64) {
	z := make([]float64, n)
	sum := 0.0 // at least 1: z[0] = 1
	for u := range n {
		z[u] = math.Pow(float64(u+1), -s)
		sum += z[u]
	}
	heavier := max(activeW, burstW)
	a, b := activeW/heavier, burstW/heavier // in (0, 1]
	scale := float64(totalWeight) / sum
	wActive, wBurst = make([]int64, n), make([]int64, n)
	for u := range n {
		wActive[u] = max(1, int64(float64(z[u]*a)*scale))
		wBurst[u] = max(1, int64(float64(z[u]*b)*scale))
	}
	return wActive, wBurst
}

// eligibleWeight is u's weight for its state and window now.
func (p *picker) eligibleWeight(u int) int64 {
	if !p.inWin[u] {
		return 0
	}
	switch p.state[u] {
	case active:
		return p.wActive[u]
	case burst:
		return p.wBurst[u]
	case quiet:
	}
	return 0
}

// update puts u's current weight into the tree.
func (p *picker) update(u int) {
	d := p.eligibleWeight(u) - p.weight[u]
	if d == 0 {
		return
	}
	p.weight[u] += d
	p.total += d
	for i := u + 1; i < len(p.tree); i += i & -i {
		p.tree[i] += d
	}
}

func (p *picker) setInWindow(u int, in bool) {
	p.inWin[u] = in
	p.update(u)
}

// advance applies every state move at or before ns.
func (p *picker) advance(ns int64) {
	if p.moves == nil {
		return
	}
	for p.moves.next() <= ns {
		u, s := p.moves.move()
		p.state[u] = s
		p.update(u)
	}
}

// find returns the user whose share of the weight line holds target, 0 ≤ target < total: the
// u with weight(0…u−1) ≤ target < weight(0…u), so its own weight is above 0.
func (p *picker) find(target int64) int {
	pos := 0
	for step := p.top; step > 0; step >>= 1 {
		if j := pos + step; j < len(p.tree) && p.tree[j] <= target {
			pos = j
			target -= p.tree[j]
		}
	}
	return pos
}

// markov moves the users between quiet, active and burst (plan choice): every user starts
// active and stays in a state for an exponential time with that state's mean_dwell_s, then
// moves to one of the other two states with equal chance. The moves come out in (time, user)
// order from one generator, so they never depend on the arrivals.
type markov struct {
	rng   *rand.Rand
	dwell [3]float64 // mean dwell per state, ns
	state []userState
	due   moveHeap // each user's next move
}

func newMarkov(seed uint64, st config.States, n int) *markov {
	m := &markov{rng: rand.New(rand.NewPCG(seed, streamMarkov)), state: make([]userState, n), due: make(moveHeap, n)}
	m.dwell[quiet], m.dwell[active], m.dwell[burst] = st.Quiet.MeanDwellS*1e9, st.Active.MeanDwellS*1e9, st.Burst.MeanDwellS*1e9
	for u := range n {
		m.state[u] = active
		m.due[u] = move{ns: m.after(0, active), user: u}
	}
	heap.Init(&m.due)
	return m
}

// after returns the time of the next move of a user that entered state s at ns (at most the
// run's end, a day): an exponential dwell, at least 1 ns, or never (math.MaxInt64) when the
// dwell reaches 10^18 ns, decades past any run.
func (m *markov) after(ns int64, s userState) int64 {
	d := max(1, math.Ceil(float64(m.rng.ExpFloat64()*m.dwell[s])))
	if d >= 1e18 {
		return math.MaxInt64
	}
	return ns + int64(d)
}

// next is the time of the earliest move.
func (m *markov) next() int64 { return m.due[0].ns }

// move makes the earliest move: the user goes to one of its two other states at random, and
// its next move is scheduled. It returns the user and its new state.
func (m *markov) move() (user int, to userState) {
	mv := &m.due[0]
	to = otherStates(m.state[mv.user])[m.rng.IntN(2)]
	m.state[mv.user] = to
	mv.ns = m.after(mv.ns, to)
	user = mv.user
	heap.Fix(&m.due, 0)
	return user, to
}

// otherStates returns the two states a user in state s can move to.
func otherStates(s userState) [2]userState {
	switch s {
	case quiet:
		return [2]userState{active, burst}
	case active:
		return [2]userState{burst, quiet}
	case burst:
	}
	return [2]userState{quiet, active}
}

// move is a user's next state move.
type move struct {
	ns   int64
	user int
}

// moveHeap orders the users' next moves by (time, user).
type moveHeap []move

func (h moveHeap) Len() int { return len(h) }
func (h moveHeap) Less(i, j int) bool {
	if h[i].ns != h[j].ns {
		return h[i].ns < h[j].ns
	}
	return h[i].user < h[j].user
}
func (h moveHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

// Push and Pop complete heap.Interface; the heap keeps one move per user, so neither is used.
func (h *moveHeap) Push(x any) { *h = append(*h, x.(move)) }
func (h *moveHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// scheduleLine is one schedule.jsonl line (plan H2 step 3): the §B4 fields (planned offset,
// user, request ID, target instance) plus i, step, kind and duplicate_of (null for an initial
// entry; plan choice).
type scheduleLine struct {
	I           int    `json:"i"`
	PlannedNs   int64  `json:"planned_ns"`
	Step        int    `json:"step"`
	User        string `json:"user"`
	RequestID   string `json:"request_id"`
	Instance    int    `json:"instance"`
	Kind        string `json:"kind"`
	DuplicateOf *int   `json:"duplicate_of"`
}

// writeSchedule writes entries to path as schedule.jsonl, one JSON line each, replacing any
// earlier file, and syncs it: hitgen writes it before it sends anything, and the orchestrator
// hashes it into context.json (TRF-1, H3). The bytes depend only on the entries.
func writeSchedule(path string, entries []Entry) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating the schedule: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing the schedule: %w", cerr)
		}
	}()
	w := bufio.NewWriterSize(f, 1<<16)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		line := scheduleLine{I: e.I, PlannedNs: e.PlannedNs, Step: e.Step, User: e.User, RequestID: e.RequestID,
			Instance: e.Instance, Kind: e.Kind}
		if e.DuplicateOf >= 0 {
			of := e.DuplicateOf
			line.DuplicateOf = &of
		}
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("writing the schedule: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the schedule: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing the schedule: %w", err)
	}
	return nil
}
