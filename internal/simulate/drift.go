package simulate

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/5stackgg/demo-parser/internal/geometry"
)

// Map-patch drift detection: which stored lineups did this map update break.
//
// Every lineup is flown twice with the same seed and the same constants, once
// against the mesh from before the patch and once against the mesh from after
// it. Nothing is claimed about either landing on its own — see the package doc
// — only about the vector between them, which is where the map moved under the
// lineup.

// Bounds on one request. A flight is a thousand-odd raycasts against a mesh of
// a few hundred thousand triangles, and this process also parses demos.
const (
	// MaxLineups is the most one request may carry, streaming or not. A lineup
	// costs two flights, measured at ~2 ms on one core and ~0.4 ms across eight
	// (BenchmarkDriftBatchOnARealMesh, de_mirage), so the cap is twenty seconds
	// of a single core and about four across a pool — comfortably inside the
	// server's write timeout, with the response streamed.
	MaxLineups = 10000
	// MaxBufferedLineups is the most that will be answered in a single JSON
	// body. Past it the whole response is held in memory before a byte is
	// written, so a bigger batch has to stream (or be chunked by the caller).
	MaxBufferedLineups = 2000
	// chunkSize is how many lineups are simulated between emissions. Big
	// enough that the worker pool stays busy, small enough that a streaming
	// client sees progress and the in-flight slice stays trivial.
	chunkSize = 128
)

// Thresholds are the cuts between verdicts, in source units.
//
// THESE ARE JUDGEMENTS, NOT MEASUREMENTS. There is no experiment that says a
// lineup moving 7 units is fine and one moving 9 is not; these are the numbers
// that make the report useful to a human reviewing a map patch.
type Thresholds struct {
	// Unchanged is how far a landing may move and still count as noise.
	//
	// The two meshes are re-exports of a map that mostly did not change, but
	// they are separate float32 files: a vertex can round differently, a
	// triangle can be split differently, and a bounce grazing that seam comes
	// off a fraction of a degree apart and lands a few units away. Eight units
	// is a quarter of a player's width and half a step height — below anything
	// a thrower could act on, and above the seam noise.
	Unchanged float64 `json:"unchanged"`
	// Major is where a move stops being a nudge. A smoke's radius is 144, a
	// player is 32 wide: past 64 units a cloud no longer covers the same gap
	// and a pop-flash no longer blinds the same doorway. Below it the lineup
	// probably still does its job and wants an eyeball; above it, it does not.
	Major float64 `json:"major"`
}

// DefaultThresholds — see the Thresholds doc for why these numbers.
func DefaultThresholds() Thresholds {
	return Thresholds{Unchanged: 8.0, Major: 64.0}
}

// Validate rejects a threshold pair that cannot be applied.
func (t Thresholds) Validate() error {
	if t.Unchanged < 0 || t.Major < 0 {
		return errors.New("thresholds must not be negative")
	}
	if t.Major < t.Unchanged {
		return errors.New("thresholds.major must be at least thresholds.unchanged")
	}
	return nil
}

// LineupSeed is one stored lineup as the library holds it. The seed is
// optional because it is: lineups mined out of demos before 5stack started
// recording throws have a landing spot and no way to reproduce the throw.
// Those are reported unsimulatable rather than guessed at.
type LineupSeed struct {
	ID              string `json:"id"`
	NadeType        string `json:"nade_type"`
	InitialPosition *Point `json:"initial_position,omitempty"`
	InitialVelocity *Point `json:"initial_velocity,omitempty"`
}

// Verdict is what the differential says about one lineup.
type Verdict string

const (
	// VerdictUnchanged — both meshes put the grenade in the same place, within
	// Thresholds.Unchanged. The map did not change under this lineup.
	VerdictUnchanged Verdict = "unchanged"
	// VerdictMoved — both meshes resolve, and the two endpoints are apart.
	// Severity says how far.
	VerdictMoved Verdict = "moved"
	// VerdictBroken — the lineup resolved on the old mesh and does not on the
	// new one: it is now inside geometry, off the map, or never settles.
	VerdictBroken Verdict = "broken"
	// VerdictUnsimulatable — nothing can be said. No recorded seed, an unknown
	// grenade type, or a flight that fails to resolve on BOTH meshes, which
	// says something is wrong with the seed or the model rather than the map.
	VerdictUnsimulatable Verdict = "unsimulatable"
)

// LineupDrift is one lineup's answer.
type LineupDrift struct {
	// Index is the lineup's position in the request, so results can be matched
	// up even when ids are missing or duplicated.
	Index   int     `json:"index"`
	ID      string  `json:"id,omitempty"`
	Verdict Verdict `json:"verdict"`
	// Reason is why, for anything other than unchanged. Stable enough to
	// switch on, but the verdict is the field to branch on.
	Reason string `json:"reason,omitempty"`
	// Severity is "minor" or "major" on a moved lineup, and empty otherwise.
	Severity string `json:"severity,omitempty"`
	// From and To are the two flights. Present whenever the flight ran at all,
	// including when it did not resolve — the stop reason is the useful part
	// of a broken lineup. Absent on unsimulatable seeds, which never flew.
	From *ComparableOutcome `json:"from,omitempty"`
	To   *ComparableOutcome `json:"to,omitempty"`
	// Distance, DistanceXY and DistanceZ are how far the endpoint moved,
	// source units. Null unless BOTH flights resolved: the gap between a real
	// landing and a flight that fell out of the map is not a distance that
	// means anything.
	Distance   *float64 `json:"distance,omitempty"`
	DistanceXY *float64 `json:"distance_xy,omitempty"`
	DistanceZ  *float64 `json:"distance_z,omitempty"`
}

// DriftRequest asks which of a batch of lineups a map patch moved.
type DriftRequest struct {
	Map string `json:"map"`
	// From and To name the mesh revisions to compare — a jsDelivr tag
	// ("17595823-4"), an owner/repo@tag, or an http(s) base. Empty means the
	// revision this process is pinned to, which is the useful spelling for To
	// right after a deploy.
	From string `json:"from"`
	To   string `json:"to"`
	// Lineups is the batch. Order is preserved in the response.
	Lineups []LineupSeed `json:"lineups"`
	// Constants overrides individual physics knobs. Any override applies to
	// BOTH sides — that is the invariant the whole method rests on, so it is
	// not expressible per side.
	Constants *ConstantOverrides `json:"constants,omitempty"`
	// UnchangedRadius and MajorRadius override the verdict cuts. They are
	// request fields because they are judgements, not measurements.
	UnchangedRadius *float64 `json:"unchanged_radius,omitempty"`
	MajorRadius     *float64 `json:"major_radius,omitempty"`
	// Stream asks for NDJSON instead of one JSON body. Required above
	// MaxBufferedLineups.
	Stream bool `json:"stream,omitempty"`
}

// Thresholds is the verdict cuts this request will be judged against —
// exported so a streaming caller can be told them before the first result is
// computed, without recomputing the defaulting rules somewhere else.
func (r DriftRequest) Thresholds() Thresholds {
	t := DefaultThresholds()
	if r.UnchangedRadius != nil {
		t.Unchanged = *r.UnchangedRadius
	}
	if r.MajorRadius != nil {
		t.Major = *r.MajorRadius
	}
	return t
}

// DriftSummary is the batch-level count, which is what a reviewer reads first.
type DriftSummary struct {
	Lineups       int `json:"lineups"`
	Unchanged     int `json:"unchanged"`
	Moved         int `json:"moved"`
	Broken        int `json:"broken"`
	Unsimulatable int `json:"unsimulatable"`
	// MaxDistance is the largest move among lineups that resolved on both
	// sides. Zero when none did.
	MaxDistance float64 `json:"max_distance"`
}

func (s *DriftSummary) add(d LineupDrift) {
	s.Lineups++
	switch d.Verdict {
	case VerdictUnchanged:
		s.Unchanged++
	case VerdictMoved:
		s.Moved++
	case VerdictBroken:
		s.Broken++
	default:
		s.Unsimulatable++
	}
	if d.Distance != nil && *d.Distance > s.MaxDistance {
		s.MaxDistance = *d.Distance
	}
}

// DriftResponse is the whole answer. Results is empty when the caller streamed.
type DriftResponse struct {
	Map        string        `json:"map"`
	From       string        `json:"from"`
	To         string        `json:"to"`
	Constants  Constants     `json:"constants"`
	Thresholds Thresholds    `json:"thresholds"`
	Summary    DriftSummary  `json:"summary"`
	Results    []LineupDrift `json:"results,omitempty"`
	// Caveats travels with the payload on purpose. Everything downstream of
	// here is a screen someone reads, and the one thing they must not conclude
	// from it is that these coordinates are where a grenade lands.
	Caveats []string `json:"caveats"`
}

// DriftCaveats is what a consumer of this endpoint has to be told, every time.
func DriftCaveats() []string {
	return []string{
		"comparison points are simulator output, not real landings: the physics model is " +
			"approximate and unfitted, and no coordinate here may be shown to a player as " +
			"where their nade lands",
		"only the difference between the two runs is meaningful; a constant model error " +
			"appears on both sides and cancels",
		"a lineup with no recorded initial_velocity is unsimulatable, not unchanged",
		"an 'unchanged' verdict means the collision mesh did not move under this lineup — " +
			"it says nothing about textures, clipping, or anything the .tri does not carry",
	}
}

// DriftOptions are the server-side knobs, kept out of the request body because
// they are this process's business and not the caller's.
type DriftOptions struct {
	// Workers bounds how many flights run at once. Zero or less means one.
	// Flights are independent and each is deterministic, so the pool changes
	// throughput and nothing else.
	Workers int
	// Emit, when set, receives results in request order as chunks complete,
	// and Results is left empty on the response. This is what makes a batch of
	// thousands answerable without buffering the whole thing.
	//
	// The slice is reused between chunks: write it out or copy it, never keep
	// it. Returning an error stops the run and is returned from Drift.
	Emit func([]LineupDrift) error
}

// Drift flies every lineup against both meshes and reports what moved.
//
// from and to must be the SAME map at two revisions. Nothing here can check
// that — two unrelated meshes will produce a report saying every lineup broke,
// which is technically true and useless.
func Drift(from, to *geometry.Mesh, req DriftRequest, opts DriftOptions) (DriftResponse, error) {
	if from == nil || from.Triangles() == 0 || to == nil || to.Triangles() == 0 {
		return DriftResponse{}, ErrNoMesh
	}
	if len(req.Lineups) == 0 {
		return DriftResponse{}, errors.New("lineups must not be empty")
	}
	if len(req.Lineups) > MaxLineups {
		return DriftResponse{}, fmt.Errorf("too many lineups: %d (max %d)", len(req.Lineups), MaxLineups)
	}
	consts := req.Constants.Apply(DefaultConstants())
	if err := consts.Validate(); err != nil {
		return DriftResponse{}, fmt.Errorf("constants: %w", err)
	}
	thresholds := req.Thresholds()
	if err := thresholds.Validate(); err != nil {
		return DriftResponse{}, err
	}

	out := DriftResponse{
		Map:        req.Map,
		From:       req.From,
		To:         req.To,
		Constants:  consts,
		Thresholds: thresholds,
		Caveats:    DriftCaveats(),
	}
	if opts.Emit == nil {
		out.Results = make([]LineupDrift, 0, len(req.Lineups))
	}

	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > chunkSize {
		workers = chunkSize
	}

	// Chunked rather than one big pool: results stay in request order without
	// any reordering step, the streaming and buffered paths are the same code,
	// and at most chunkSize results exist at once.
	buf := make([]LineupDrift, 0, chunkSize)
	for start := 0; start < len(req.Lineups); start += chunkSize {
		end := min(start+chunkSize, len(req.Lineups))
		batch := req.Lineups[start:end]
		buf = buf[:len(batch)]

		var (
			wg   sync.WaitGroup
			next atomic.Int64
		)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1)) - 1
					if i >= len(batch) {
						return
					}
					// Each worker owns the index it claimed, so the results
					// land in request order with no reordering pass.
					buf[i] = driftOne(from, to, start+i, batch[i], consts, thresholds)
				}
			}()
		}
		wg.Wait()

		for i := range buf {
			out.Summary.add(buf[i])
		}
		if opts.Emit != nil {
			if err := opts.Emit(buf); err != nil {
				return out, err
			}
			continue
		}
		out.Results = append(out.Results, buf...)
	}
	return out, nil
}

// driftOne is the whole verdict, for one lineup.
func driftOne(fromMesh, toMesh *geometry.Mesh, index int, seed LineupSeed, c Constants, t Thresholds) LineupDrift {
	d := LineupDrift{Index: index, ID: seed.ID}

	nade, ok := ParseNadeType(seed.NadeType)
	if !ok {
		return unsimulatable(d, fmt.Sprintf("unknown nade_type %q", seed.NadeType))
	}
	if seed.InitialPosition == nil || seed.InitialVelocity == nil {
		// The common case for a demo-mined lineup: we know where it landed and
		// nothing about how it was thrown. Saying "unchanged" here would be a
		// lie of omission, and guessing a throw would be a plain lie.
		return unsimulatable(d, "no recorded initial_position/initial_velocity: lineup cannot be re-simulated")
	}
	flight := Seed{
		Type:     nade,
		Position: seed.InitialPosition.vec(),
		Velocity: seed.InitialVelocity.vec(),
	}

	before, err := SimulateForComparison(fromMesh, flight, c)
	if err != nil {
		return unsimulatable(d, err.Error())
	}
	after, err := SimulateForComparison(toMesh, flight, c)
	if err != nil {
		return unsimulatable(d, err.Error())
	}
	d.From, d.To = &before, &after

	switch {
	case !before.Resolved && !after.Resolved:
		// The map is not the problem: the same thing happens on both meshes.
		d.Verdict = VerdictUnsimulatable
		d.Reason = fmt.Sprintf("flight does not resolve on either mesh (%s): bad seed or a limit of the model", after.Stop)
		return d
	case before.Resolved && !after.Resolved:
		d.Verdict = VerdictBroken
		d.Reason = brokenReason(after.Stop)
		return d
	case !before.Resolved && after.Resolved:
		// Backwards drift: the lineup did not work on the old mesh and does on
		// the new one. Rare, and still a change the reviewer wants to see, so
		// it is reported as moved with the reason spelled out rather than
		// hidden under "unchanged".
		d.Verdict = VerdictMoved
		d.Severity = "major"
		d.Reason = fmt.Sprintf("did not resolve on the old mesh (%s) but does on the new one", before.Stop)
		return d
	}

	dx := after.ComparisonPoint.X - before.ComparisonPoint.X
	dy := after.ComparisonPoint.Y - before.ComparisonPoint.Y
	dz := after.ComparisonPoint.Z - before.ComparisonPoint.Z
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	xy := math.Hypot(dx, dy)
	z := math.Abs(dz)
	d.Distance, d.DistanceXY, d.DistanceZ = &dist, &xy, &z

	if dist < t.Unchanged {
		d.Verdict = VerdictUnchanged
		return d
	}
	d.Verdict = VerdictMoved
	d.Severity = "minor"
	if dist >= t.Major {
		d.Severity = "major"
	}
	d.Reason = fmt.Sprintf("landing moved %.1f units", dist)
	return d
}

func unsimulatable(d LineupDrift, reason string) LineupDrift {
	d.Verdict = VerdictUnsimulatable
	d.Reason = reason
	return d
}

func brokenReason(stop StopReason) string {
	switch stop {
	case StopInsideGeometry:
		return "landing is now inside geometry"
	case StopStartSealed:
		return "the throw position is now inside geometry"
	case StopOutOfWorld:
		return "the grenade now leaves the map"
	case StopMaxFlight:
		return "the grenade no longer comes to rest"
	}
	return fmt.Sprintf("no longer resolves (%s)", stop)
}
