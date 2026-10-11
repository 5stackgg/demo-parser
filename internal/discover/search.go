package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// Limits on one search. They bound the work, not the answer: a bigger area is
// searched at a wider spacing rather than refused.
const (
	MaxStands      = 400
	MaxTechniques  = 16
	DefaultSpacing = 32.0
	MinSpacing     = 8.0
	DefaultLimit   = 40
	MaxLimit       = 200
	DefaultBudget  = 5 * time.Second
	MaxBudget      = 30 * time.Second

	// No map is bigger than this in any direction, and nothing a player can
	// stand on is further out. Requests outside it are not searched.
	worldExtent = 65536.0
	// The widest area one search covers, per side.
	maxAreaSide = 4096.0

	// A player is 72 units tall and 32 across.
	playerHeight = 72.0
	playerHalf   = 16.0

	// The coarse pass sweeps the view pitch at this step, from straight up
	// to well below the horizon, and the yaw around the bearing to the
	// target: a smoke that bounces before it settles lands off the line it
	// was thrown along, and a lineup's aim can sit degrees either side of it.
	coarseStep = 3.0
	minPitch   = -89.0
	maxPitch   = 60.0

	// How much aim error a throw is tested against: about what a careful
	// player gets lining a crosshair up against a reference.
	aimJitter = 0.25

	// How far from the target a first impact can be and the grenade still
	// end up there. Bounces lose more than half the speed each time, so a
	// smoke settles within a few hundred units of where it first lands; a
	// first impact farther out than this is not worth flying to rest.
	bounceReach = 768.0

	maxDipsPerStance = 3
	maxDescentMoves  = 12
)

// Area is where the throw may be made from: a box on the map, its Z range
// picking the floor on a map with more than one, and optionally a circle
// inside it.
type Area struct {
	Min    r3.Vector `json:"min"`
	Max    r3.Vector `json:"max"`
	Radius float64   `json:"radius,omitempty"`
}

// Request is one search.
type Request struct {
	Map        string      `json:"map"`
	Revision   string      `json:"revision,omitempty"`
	Target     r3.Vector   `json:"target"`
	Radius     float64     `json:"radius"`
	Area       Area        `json:"area"`
	Techniques []Technique `json:"techniques"`
	Strengths  []Strength  `json:"strengths"`
	Spacing    float64     `json:"spacing,omitempty"`
	Limit      int         `json:"limit,omitempty"`
	BudgetMs   int         `json:"budget_ms,omitempty"`
}

func inWorld(p r3.Vector) bool {
	for _, c := range [3]float64{p.X, p.Y, p.Z} {
		if math.IsNaN(c) || math.IsInf(c, 0) || math.Abs(c) > worldExtent {
			return false
		}
	}
	return true
}

// Validate fills the defaults and refuses what cannot be searched. The work a
// search does is stands x techniques x strengths, so each of those is bounded
// here, and duplicates are dropped rather than searched twice.
func (r *Request) Validate() error {
	if math.IsNaN(r.Radius) || r.Radius <= 0 || r.Radius > 512 {
		return errors.New("radius must be between 0 and 512")
	}
	if !inWorld(r.Target) || !inWorld(r.Area.Min) || !inWorld(r.Area.Max) {
		return errors.New("target and area must be finite points on the map")
	}
	if r.Area.Max.X < r.Area.Min.X || r.Area.Max.Y < r.Area.Min.Y || r.Area.Max.Z < r.Area.Min.Z {
		return errors.New("area max must not be below its min")
	}
	if r.Area.Max.X-r.Area.Min.X > maxAreaSide || r.Area.Max.Y-r.Area.Min.Y > maxAreaSide {
		return fmt.Errorf("area must be at most %.0f units a side", maxAreaSide)
	}
	if math.IsNaN(r.Area.Radius) || r.Area.Radius < 0 || r.Area.Radius > maxAreaSide {
		return errors.New("area radius must be between 0 and the area's size")
	}

	techniques := make([]Technique, 0, len(r.Techniques))
	for _, t := range r.Techniques {
		if !t.valid() {
			return fmt.Errorf("technique %+v is not one a player can do", t)
		}
		if !slices.Contains(techniques, t) {
			techniques = append(techniques, t)
		}
	}
	if len(techniques) == 0 || len(techniques) > MaxTechniques {
		return fmt.Errorf("between 1 and %d techniques", MaxTechniques)
	}
	r.Techniques = techniques

	strengths := make([]Strength, 0, 3)
	for _, s := range r.Strengths {
		if !s.valid() {
			return fmt.Errorf("strength %q is not Full, Half or Drop", s)
		}
		if !slices.Contains(strengths, s) {
			strengths = append(strengths, s)
		}
	}
	if len(strengths) == 0 {
		strengths = append(strengths, Full)
	}
	r.Strengths = strengths

	if math.IsNaN(r.Spacing) || math.IsInf(r.Spacing, 0) || r.Spacing < 0 {
		return errors.New("spacing must be a positive number of units")
	}
	if r.Spacing == 0 {
		r.Spacing = DefaultSpacing
	}
	if r.Spacing < MinSpacing {
		r.Spacing = MinSpacing
	}
	if r.Limit <= 0 {
		r.Limit = DefaultLimit
	}
	if r.Limit > MaxLimit {
		r.Limit = MaxLimit
	}
	return nil
}

func (r Request) budget() time.Duration {
	if r.BudgetMs <= 0 {
		return DefaultBudget
	}
	b := time.Duration(r.BudgetMs) * time.Millisecond
	if b > MaxBudget {
		return MaxBudget
	}
	return b
}

// Candidate is a throw the model thinks lands on the target. A game server
// throws it for real before anyone is shown it as anything but predicted.
type Candidate struct {
	Stand     r3.Vector `json:"stand"`
	Eye       r3.Vector `json:"eye"`
	Technique Technique `json:"technique"`
	Name      string    `json:"technique_name"`
	Measured  bool      `json:"measured"`
	Strength  Strength  `json:"strength"`
	Pitch     float64   `json:"pitch"`
	Yaw       float64   `json:"yaw"`
	Position  r3.Vector `json:"position"`
	Velocity  r3.Vector `json:"velocity"`
	Landing   r3.Vector `json:"landing"`
	Distance  float64   `json:"distance"`
	Bounces   int       `json:"bounces"`
	Seconds   float64   `json:"seconds"`
	// Spread is how far the landing moves for a quarter of a degree of aim
	// error either way: the lower, the easier the throw.
	Spread float64 `json:"spread"`
}

// point is a vector as it goes over the wire. r3.Vector has no JSON tags, so
// it would go out as X/Y/Z, and every reader of this expects x/y/z.
type point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func wire(v r3.Vector) point {
	return point{X: v.X, Y: v.Y, Z: v.Z}
}

// MarshalJSON writes the vectors as points, and a spread that could not be
// measured (a jittered throw that never settled) as null: encoding/json
// refuses +Inf and would fail the whole response.
func (c Candidate) MarshalJSON() ([]byte, error) {
	var spread *float64
	if !math.IsInf(c.Spread, 0) && !math.IsNaN(c.Spread) {
		spread = &c.Spread
	}
	return json.Marshal(struct {
		Stand     point     `json:"stand"`
		Eye       point     `json:"eye"`
		Technique Technique `json:"technique"`
		Name      string    `json:"technique_name"`
		Measured  bool      `json:"measured"`
		Strength  Strength  `json:"strength"`
		Pitch     float64   `json:"pitch"`
		Yaw       float64   `json:"yaw"`
		Position  point     `json:"position"`
		Velocity  point     `json:"velocity"`
		Landing   point     `json:"landing"`
		Distance  float64   `json:"distance"`
		Bounces   int       `json:"bounces"`
		Seconds   float64   `json:"seconds"`
		Spread    *float64  `json:"spread"`
	}{
		Stand:     wire(c.Stand),
		Eye:       wire(c.Eye),
		Technique: c.Technique,
		Name:      c.Name,
		Measured:  c.Measured,
		Strength:  c.Strength,
		Pitch:     c.Pitch,
		Yaw:       c.Yaw,
		Position:  wire(c.Position),
		Velocity:  wire(c.Velocity),
		Landing:   wire(c.Landing),
		Distance:  c.Distance,
		Bounces:   c.Bounces,
		Seconds:   c.Seconds,
		Spread:    spread,
	})
}

// Response is what a search found.
type Response struct {
	Candidates []Candidate `json:"candidates"`
	Stands     int         `json:"stands"`
	Flights    int         `json:"flights"`
	Searched   int         `json:"searched"`
	Truncated  bool        `json:"truncated"`
	Revision   string      `json:"revision"`
}

// stance is a spot in the area a player can stand.
type stance struct {
	feet r3.Vector
}

// Stands are the places in the area a player fits, on the floor the area's Z
// range names, at the request's spacing (widened until there are no more than
// MaxStands).
func Stands(world *geometry.GrenadeWorld, area Area, spacing float64) []r3.Vector {
	hull := world.Hull()
	if hull == nil {
		return nil
	}
	// Bounded here as well as in Validate: this is exported, and the grid it
	// walks is the one thing in a search that grows with its inputs.
	if !inWorld(area.Min) || !inWorld(area.Max) || math.IsNaN(spacing) {
		return nil
	}
	if area.Max.X-area.Min.X > maxAreaSide || area.Max.Y-area.Min.Y > maxAreaSide ||
		area.Max.X < area.Min.X || area.Max.Y < area.Min.Y {
		return nil
	}
	spacing = math.Max(spacing, MinSpacing)
	// Counted, not stepped: a float walked from one bound to the other can
	// stop moving long before it gets there.
	nx := int(math.Floor((area.Max.X-area.Min.X)/spacing)) + 1
	ny := int(math.Floor((area.Max.Y-area.Min.Y)/spacing)) + 1
	for nx*ny > MaxStands*4 {
		spacing *= 1.5
		nx = int(math.Floor((area.Max.X-area.Min.X)/spacing)) + 1
		ny = int(math.Floor((area.Max.Y-area.Min.Y)/spacing)) + 1
	}
	centre := area.Min.Add(area.Max).Mul(0.5)
	var out []r3.Vector
	for i := 0; i < nx; i++ {
		x := area.Min.X + float64(i)*spacing
		for j := 0; j < ny; j++ {
			y := area.Min.Y + float64(j)*spacing
			if area.Radius > 0 && math.Hypot(x-centre.X, y-centre.Y) > area.Radius {
				continue
			}
			feet, ok := floorAt(hull, x, y, area.Min.Z, area.Max.Z)
			if !ok || !fits(hull, feet) {
				continue
			}
			out = append(out, feet)
		}
	}
	if len(out) > MaxStands {
		step := float64(len(out)) / MaxStands
		thinned := make([]r3.Vector, 0, MaxStands)
		for i := 0.0; int(i) < len(out) && len(thinned) < MaxStands; i += step {
			thinned = append(thinned, out[int(i)])
		}
		out = thinned
	}
	return out
}

// floorAt is the walkable floor under (x, y) within [minZ, maxZ], looking
// down from a player's height above the range so a ceiling over it is not
// mistaken for the floor. A player is a box, so it stands on the highest floor
// under any part of it: on a stair edge that is the step, where a single ray
// down the middle would fall to the one below.
func floorAt(hull *geometry.Mesh, x, y, minZ, maxZ float64) (r3.Vector, bool) {
	top := maxZ + playerHeight
	best := math.Inf(-1)
	inset := playerHalf - 1
	for _, off := range [5][2]float64{{0, 0}, {inset, inset}, {inset, -inset}, {-inset, inset}, {-inset, -inset}} {
		from := r3.Vector{X: x + off[0], Y: y + off[1], Z: top}
		hit, ok := hull.RayHitSurface(from, r3.Vector{Z: -1})
		if !ok || hit.Normal.Z < 0.7 {
			continue
		}
		z := top - hit.Distance
		if z < minZ-8 || z > maxZ+8 {
			continue
		}
		best = math.Max(best, z)
	}
	if math.IsInf(best, -1) {
		return r3.Vector{}, false
	}
	return r3.Vector{X: x, Y: y, Z: best}, true
}

// fits is whether a standing player has room at feet: headroom, and no wall
// inside their own width.
func fits(hull *geometry.Mesh, feet r3.Vector) bool {
	if d, ok := hull.RayHitDist(feet.Add(r3.Vector{Z: 1}), r3.Vector{Z: 1}); ok && d < playerHeight {
		return false
	}
	waist := feet.Add(r3.Vector{Z: playerHeight / 2})
	for _, dir := range []r3.Vector{{X: 1}, {X: -1}, {Y: 1}, {Y: -1}} {
		if d, ok := hull.RayHitDist(waist, dir); ok && d < playerHalf {
			return false
		}
	}
	return true
}

type throwSpec struct {
	stand     r3.Vector
	technique Technique
	strength  Strength
}

// aimed is one evaluated aim.
type aimed struct {
	pitch, yaw float64
	flight     Flight
	distance   float64
}

// yawFor is the yaw that sends the grenade at the target. Standing or
// moving straight along the facing that is the bearing; a strafe pushes the
// grenade sideways, so the facing has to lead it.
func yawFor(spec throwSpec, pitch float64, target r3.Vector) float64 {
	bearing := math.Atan2(target.Y-spec.stand.Y, target.X-spec.stand.X) * 180 / math.Pi
	yaw := bearing
	for i := 0; i < 3; i++ {
		_, v := spec.technique.Seed(spec.stand, pitch, yaw, spec.strength)
		heading := math.Atan2(v.Y, v.X) * 180 / math.Pi
		yaw += angleDelta(bearing, heading)
	}
	return yaw
}

func angleDelta(a, b float64) float64 {
	d := math.Mod(a-b, 360)
	if d > 180 {
		d -= 360
	} else if d < -180 {
		d += 360
	}
	return d
}

type searcher struct {
	world  *geometry.GrenadeWorld
	fine   Model
	coarse Model
	target r3.Vector
	radius float64

	flights atomic.Int64
}

func (s *searcher) fly(m Model, spec throwSpec, pitch, yaw float64) aimed {
	pos, vel := spec.technique.Seed(spec.stand, pitch, yaw, spec.strength)
	f := m.Fly(s.world, pos, vel, false)
	s.flights.Add(1)
	d := math.Inf(1)
	if f.Settled {
		d = f.Rest.Sub(s.target).Norm()
	}
	return aimed{pitch: pitch, yaw: yaw, flight: f, distance: d}
}

var yawOffsets = []float64{-11, -7, -4, -2, 0, 2, 4, 7, 11}

// flyIfReachable is a coarse flight, skipped when the first impact is too
// far from the target for the bounces to bring it back.
func (s *searcher) flyIfReachable(spec throwSpec, pitch, yaw float64) aimed {
	pos, vel := spec.technique.Seed(spec.stand, pitch, yaw, spec.strength)
	impact, hit := s.coarse.Impact(s.world, pos, vel)
	if !hit || math.Hypot(impact.X-s.target.X, impact.Y-s.target.Y) > bounceReach {
		return aimed{pitch: pitch, yaw: yaw, distance: math.Inf(1)}
	}
	return s.fly(s.coarse, spec, pitch, yaw)
}

// search finds the aims from one stance with one technique and button that
// the model lands near the target: a coarse sweep of pitch and yaw with the
// cheap trace, then a descent from every dip in it with the full one.
func (s *searcher) search(ctx context.Context, spec throwSpec) []Candidate {
	var grid [][]aimed
	for pitch := minPitch; pitch <= maxPitch; pitch += coarseStep {
		if ctx.Err() != nil {
			return nil
		}
		centre := yawFor(spec, pitch, s.target)
		row := make([]aimed, len(yawOffsets))
		for j, off := range yawOffsets {
			row[j] = s.flyIfReachable(spec, pitch, centre+off)
		}
		grid = append(grid, row)
	}

	// The closest few local dips are worth a closer look. The coarse trace
	// is a ranking, so "close" is generous; a stance rarely has more than a
	// couple of real lineups onto one spot, so the rest is noise.
	near := math.Max(s.radius*4, 256)
	var dips []aimed
	for i, row := range grid {
		for j, a := range row {
			if a.distance <= near && dip(grid, i, j) {
				dips = append(dips, a)
			}
		}
	}
	sort.SliceStable(dips, func(i, j int) bool { return dips[i].distance < dips[j].distance })
	if len(dips) > maxDipsPerStance {
		dips = dips[:maxDipsPerStance]
	}

	var out []Candidate
	for _, a := range dips {
		best := s.refine(ctx, spec, a)
		if best.distance > s.radius {
			continue
		}
		out = append(out, s.candidate(spec, best))
	}
	return out
}

// dip is whether no neighbour in the coarse grid lands closer.
func dip(grid [][]aimed, i, j int) bool {
	d := grid[i][j].distance
	for di := -1; di <= 1; di++ {
		for dj := -1; dj <= 1; dj++ {
			if di == 0 && dj == 0 {
				continue
			}
			ni, nj := i+di, j+dj
			if ni < 0 || ni >= len(grid) || nj < 0 || nj >= len(grid[ni]) {
				continue
			}
			if grid[ni][nj].distance < d {
				return false
			}
		}
	}
	return true
}

// refine walks the aim downhill in pitch and yaw at shrinking steps: most of
// the way with the cheap trace, the last of it with the full one.
func (s *searcher) refine(ctx context.Context, spec throwSpec, from aimed) aimed {
	best := s.descend(ctx, s.coarse, spec, from, []float64{1.2, 0.4})
	best = s.fly(s.fine, spec, best.pitch, best.yaw)
	return s.descend(ctx, s.fine, spec, best, []float64{0.15, 0.05})
}

func (s *searcher) descend(ctx context.Context, m Model, spec throwSpec, best aimed, steps []float64) aimed {
	for _, step := range steps {
		for moves := 0; moves < maxDescentMoves && ctx.Err() == nil; moves++ {
			improved := false
			for _, d := range [][2]float64{{step, 0}, {-step, 0}, {0, step}, {0, -step}} {
				pitch := best.pitch + d[0]
				if pitch < minPitch || pitch > 89 {
					continue
				}
				a := s.fly(m, spec, pitch, best.yaw+d[1])
				if a.distance < best.distance {
					best, improved = a, true
				}
			}
			if !improved {
				break
			}
		}
	}
	return best
}

func (s *searcher) candidate(spec throwSpec, a aimed) Candidate {
	pos, vel := spec.technique.Seed(spec.stand, a.pitch, a.yaw, spec.strength)
	spread := 0.0
	for _, d := range [][2]float64{{aimJitter, 0}, {-aimJitter, 0}, {0, aimJitter}, {0, -aimJitter}} {
		j := s.fly(s.fine, spec, a.pitch+d[0], a.yaw+d[1])
		if !j.flight.Settled {
			spread = math.Inf(1)
			break
		}
		spread = math.Max(spread, j.flight.Rest.Sub(a.flight.Rest).Norm())
	}
	return Candidate{
		Stand:     spec.stand,
		Eye:       spec.stand.Add(r3.Vector{Z: spec.technique.eye()}),
		Technique: spec.technique,
		Name:      spec.technique.Name(),
		Measured:  spec.technique.Measured(),
		Strength:  spec.strength,
		Pitch:     round(a.pitch, 3),
		Yaw:       round(normalizeYaw(a.yaw), 3),
		Position:  pos,
		Velocity:  vel,
		Landing:   a.flight.Rest,
		Distance:  round(a.distance, 1),
		Bounces:   a.flight.Bounces,
		Seconds:   round(a.flight.Seconds, 3),
		Spread:    round(spread, 1),
	}
}

func normalizeYaw(yaw float64) float64 {
	return angleDelta(yaw, 0)
}

func round(v float64, places int) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// Search runs one request against a world. It is bounded by the request's
// budget: what it has found by then is returned, marked truncated.
func Search(ctx context.Context, world *geometry.GrenadeWorld, req Request, workers int) (Response, error) {
	if world == nil || world.Triangles() == 0 {
		return Response{}, errors.New("no collision mesh to search against")
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, req.budget())
	defer cancel()

	stands := Stands(world, req.Area, req.Spacing)
	// Closest to the target first: when the budget runs out, the spots
	// nearest the smoke have been searched.
	sort.SliceStable(stands, func(i, j int) bool {
		return stands[i].Sub(req.Target).Norm() < stands[j].Sub(req.Target).Norm()
	})

	coarse := DefaultModel()
	coarse.CentreOnly = true
	s := &searcher{world: world, fine: DefaultModel(), coarse: coarse, target: req.Target, radius: req.Radius}

	var specs []throwSpec
	for _, st := range stands {
		for _, t := range req.Techniques {
			for _, str := range req.Strengths {
				specs = append(specs, throwSpec{stand: st, technique: t, strength: str})
			}
		}
	}

	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	results := make([][]Candidate, len(specs))
	searched := make([]bool, len(specs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = s.search(ctx, specs[i])
				searched[i] = ctx.Err() == nil
			}
		}()
	}
feed:
	for i := range specs {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	var all []Candidate
	done := 0
	for i := range specs {
		if searched[i] {
			done++
		}
		all = append(all, results[i]...)
	}

	return Response{
		Candidates: rank(all, req.Limit),
		Stands:     len(stands),
		Flights:    int(s.flights.Load()),
		Searched:   done,
		Truncated:  done < len(specs),
		Revision:   world.Revision(),
	}, nil
}

// rank orders the candidates easiest first and drops near-copies: the same
// technique and button from a neighbouring spot with nearly the same aim is
// one lineup, not two.
func rank(all []Candidate, limit int) []Candidate {
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Spread != b.Spread {
			return a.Spread < b.Spread
		}
		if a.Distance != b.Distance {
			return a.Distance < b.Distance
		}
		if a.Stand.X != b.Stand.X {
			return a.Stand.X < b.Stand.X
		}
		if a.Stand.Y != b.Stand.Y {
			return a.Stand.Y < b.Stand.Y
		}
		return a.Pitch < b.Pitch
	})
	var out []Candidate
	for _, c := range all {
		duplicate := false
		for _, kept := range out {
			if kept.Technique == c.Technique && kept.Strength == c.Strength &&
				kept.Stand.Sub(c.Stand).Norm() < 48 &&
				math.Abs(kept.Pitch-c.Pitch) < 1.5 && math.Abs(angleDelta(kept.Yaw, c.Yaw)) < 1.5 {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		out = append(out, c)
		if len(out) >= limit {
			break
		}
	}
	if out == nil {
		out = []Candidate{}
	}
	return out
}
