package parser

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// driveTicks runs the emit decision onFrameDone makes, over a range of ticks,
// with a grenade thrown at each of throwAt. One row per tick stands in for the
// per-player rows the real capture builds off the parser.
//
// The throw is dispatched before the frame it lands in, which is the order
// demoinfocs uses: game events fire while the frame is being parsed and
// FrameDone closes it.
func driveTicks(s *state, from, to, sampleEvery int, throwAt ...int) {
	throws := map[int]bool{}
	for _, t := range throwAt {
		throws[t] = true
	}
	for tick := from; tick <= to; tick++ {
		if throws[tick] {
			s.burstPositions(tick)
		}
		due := s.positionSampleDue(tick, sampleEvery)
		slot := s.stagePositionSlot(tick)
		slot.rows = append(slot.rows, EventPosition{Tick: tick, AttackerSteamID: "p"})
		if due || tick <= s.burstUntilTick {
			s.emitPositions(slot)
		}
	}
}

func emittedTicks(s *state) []int {
	out := make([]int, 0, len(s.res.Positions))
	for _, p := range s.res.Positions {
		out = append(out, p.Tick)
	}
	return out
}

func ticksBetween(lo, hi int) map[int]bool {
	set := map[int]bool{}
	for t := lo; t <= hi; t++ {
		set[t] = true
	}
	return set
}

// The window either side of a throw is what makes a mined lineup reproducible:
// the ~4Hz timeline continues, and every tick within ten of the release is
// emitted on top of it.
func TestPositionBurstCoversTheThrowWindow(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	driveTicks(s, 100, 170, 16, 130)

	want := ticksBetween(130-throwBurstTicks, 130+throwBurstTicks)
	// The ~4Hz clock keeps its own phase through the burst.
	for _, t := range []int{100, 116, 148, 164} {
		want[t] = true
	}

	got := emittedTicks(s)
	seen := map[int]int{}
	for _, tick := range got {
		seen[tick]++
	}
	for tick := range want {
		if seen[tick] != 1 {
			t.Fatalf("tick %d emitted %d times, want exactly once (got %v)", tick, seen[tick], got)
		}
	}
	for tick, n := range seen {
		if !want[tick] {
			t.Fatalf("tick %d emitted %d times but is neither due nor in the throw window", tick, n)
		}
	}
}

// Two throws close together share one run of full-rate ticks. Nothing may be
// emitted twice: the API writes these rows straight into a table.
func TestOverlappingThrowsDoNotDuplicateRows(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	driveTicks(s, 100, 170, 16, 130, 135)

	seen := map[int]int{}
	for _, tick := range emittedTicks(s) {
		seen[tick]++
		if seen[tick] > 1 {
			t.Fatalf("tick %d emitted twice", tick)
		}
	}
	for tick := 120; tick <= 145; tick++ {
		if seen[tick] != 1 {
			t.Fatalf("tick %d should be inside the merged window, got %d rows", tick, seen[tick])
		}
	}
}

// A throw in the first moments of a round has less history than the window
// asks for. It emits what the ring still holds rather than inventing ticks.
func TestThrowNearRoundStartEmitsWhatItHas(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	driveTicks(s, 200, 260, 16, 203)

	for _, tick := range emittedTicks(s) {
		if tick < 200 {
			t.Fatalf("emitted tick %d from before the round started", tick)
		}
	}
	for tick := 200; tick <= 213; tick++ {
		if !containsTick(s, tick) {
			t.Fatalf("tick %d is inside the window and should have been emitted", tick)
		}
	}
}

// The ring only holds the window it promises. A throw cannot reach further back
// than that, and must not read a stale slot as if it were recent.
func TestBurstIgnoresTicksThatRolledOutOfTheRing(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	// One tick staged a long time ago, then a gap, then the throw.
	slot := s.stagePositionSlot(50)
	slot.rows = append(slot.rows, EventPosition{Tick: 50})
	driveTicks(s, 300, 320, 16, 310)

	for _, tick := range emittedTicks(s) {
		if tick == 50 {
			t.Fatal("a slot that rolled out of the ring was emitted as if it were in the window")
		}
	}
}

// Nothing is emitted outside a live round, throw or not: the replay viewer skips
// freezetime and the walkaround, so those rows are pure payload.
func TestBurstStaysOutOfDeadTime(t *testing.T) {
	s := &state{res: &Result{}, liveRound: false}
	s.burstPositions(130)
	if len(s.res.Positions) != 0 || s.burstUntilTick != 0 {
		t.Fatalf("a throw outside a live round should emit nothing: %d rows, window to %d",
			len(s.res.Positions), s.burstUntilTick)
	}
}

// Bursts append ticks from behind the write head, so the array has to be put
// back in order before it goes out.
func TestPositionsEndUpInTickOrder(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	// Thrown a few ticks after a due sample, so the window reaches back past
	// rows already written. A throw that lands more than a window after the
	// last due sample appends in order and would not exercise this.
	driveTicks(s, 100, 170, 16, 122)

	if sort.SliceIsSorted(s.res.Positions, func(i, j int) bool {
		return s.res.Positions[i].Tick < s.res.Positions[j].Tick
	}) {
		t.Fatal("expected the burst to have put the array out of order; the sort would be untested")
	}
	if !s.sortPositions() {
		t.Fatal("sortPositions should report that it sorted")
	}
	for i := 1; i < len(s.res.Positions); i++ {
		if s.res.Positions[i-1].Tick > s.res.Positions[i].Tick {
			t.Fatalf("positions are out of order at %d: %d then %d",
				i, s.res.Positions[i-1].Tick, s.res.Positions[i].Tick)
		}
	}
}

func TestSortPositionsIsANoOpWithoutBursts(t *testing.T) {
	s := &state{res: &Result{}, liveRound: true}
	driveTicks(s, 100, 170, 16)
	if s.sortPositions() {
		t.Fatal("a parse with no throws should not touch the array")
	}
}

// Wire contract: the API and the web replay read these keys off the blob.
func TestBlobCarriesCrouchStateAndSchemaVersion(t *testing.T) {
	row, err := json.Marshal(EventPosition{Tick: 1, Ducked: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(row), `"ducked":true`) {
		t.Fatalf("crouch state is missing from the wire form: %s", row)
	}
	// Absent when standing, so the common row does not grow.
	row, _ = json.Marshal(EventPosition{Tick: 1})
	if strings.Contains(string(row), "ducked") {
		t.Fatalf("a standing row should not carry the flag: %s", row)
	}

	res, err := json.Marshal(&Result{SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(res), `"schema_version":2`) {
		t.Fatalf("schema version is missing from the blob: %s", res)
	}
	// Emitted even at zero, so a consumer can tell an old blob from a field it
	// forgot to read.
	old, _ := json.Marshal(&Result{})
	if !strings.Contains(string(old), `"schema_version":0`) {
		t.Fatalf("schema version must not be omitted at zero: %s", old)
	}
}

func containsTick(s *state, tick int) bool {
	for _, p := range s.res.Positions {
		if p.Tick == tick {
			return true
		}
	}
	return false
}
