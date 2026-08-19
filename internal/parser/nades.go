package parser

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// The lineup side of the smoke model.
//
// buildSmokeVolume is a pure function of (mesh, point): it never looks at the
// demo, only at the map. So the exact cloud a demo-mined smoke produced can
// also be produced for a point nobody has ever thrown at — which is what a
// lineup library needs, since a lineup is a throw description, not a replay.
//
// Everything here reuses that one function rather than approximating it, so a
// bloom previewed in the browser, a bloom drawn on the 2D radar, and the volume
// the parser's own sightline stats were computed against are the same grid.

// Point is a world position in raw CS2 source units (Z up) — the same space as
// PositionEyes(), the .tri meshes, and the smoke volumes.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (p Point) vec() r3.Vector {
	return r3.Vector{X: p.X, Y: p.Y, Z: p.Z}
}

func (p Point) valid() bool {
	for _, c := range [3]float64{p.X, p.Y, p.Z} {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return false
		}
	}
	return true
}

// DefaultBlockThreshold is the optical depth at which a sightline counts as
// blocked, in cell widths of fully dense smoke.
//
// It is the same constant the parser's own stats use (blockingDepth), so the
// answer this service gives and the answer baked into a parsed match agree.
// Read as Beer-Lambert: e^-3 leaves about 5% of the target's contrast, which is
// the point a silhouette stops being usable. It is a request field because it
// is a judgement, not a measurement — a caller who wants "could a good player
// have picked them out" should raise it, and one asking "was the model clean"
// should lower it.
const DefaultBlockThreshold = blockingDepth

// Bounds on what one request may ask for. This runs as a shared service, and a
// volume build is thousands of raycasts against a mesh with a hundred thousand
// triangles.
const (
	maxSightlinePairs = 512
	maxRequestClouds  = 16
	// A supplied grid is decoded into memory, so its cell count is capped. The
	// parser's own grids are 19³; this leaves room for a coarser voxel size
	// over a much larger cloud without letting a request allocate freely.
	maxSuppliedCells = 1 << 20
)

var (
	// ErrNoMesh means the map has no published collision mesh, so nothing here
	// can be answered — a smoke's shape is a property of the map.
	ErrNoMesh = errors.New("no collision mesh for map")
	// ErrSmokeSealed means the flood found almost no free space around the
	// point: it resolved inside geometry, or on a surface the mesh is missing.
	// The caller gave a point a grenade could not come to rest at.
	ErrSmokeSealed = errors.New("smoke point is sealed inside geometry")
)

// SmokeVolumeRequest asks for the bloom at one point on one map.
type SmokeVolumeRequest struct {
	Map string  `json:"map"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	Z   float64 `json:"z"`
}

func (r SmokeVolumeRequest) point() Point {
	return Point{X: r.X, Y: r.Y, Z: r.Z}
}

// SmokeVolumeResponse carries the EventSmokeVolume fields inline, so the blob
// is byte-for-byte the shape the playback pipeline already renders (ox/oy/oz,
// vs, dx/dy/dz, den) and a consumer can share one decoder for both.
type SmokeVolumeResponse struct {
	Map string `json:"map"`
	EventSmokeVolume
	// Cells is how many voxels hold any smoke at all, before the nibble
	// packing. A rough measure of how much space the cloud found.
	Cells int `json:"cells"`
	// Radius is the reach the cloud was flooded to, so a caller can size a
	// preview without hardcoding the constant.
	Radius float64 `json:"radius"`
}

// SmokeVolume computes the bloom for a point. mesh must be the map's collision
// mesh; a nil mesh is ErrNoMesh, since without geometry there is no shape.
func SmokeVolume(mesh *geometry.Mesh, req SmokeVolumeRequest) (SmokeVolumeResponse, error) {
	if mesh == nil {
		return SmokeVolumeResponse{}, ErrNoMesh
	}
	at := req.point()
	if !at.valid() {
		return SmokeVolumeResponse{}, errors.New("x, y and z must be finite")
	}
	vol, sealed := buildSmokeVolume(mesh, at.vec())
	if sealed || vol == nil {
		return SmokeVolumeResponse{}, ErrSmokeSealed
	}
	return SmokeVolumeResponse{
		Map:              req.Map,
		EventSmokeVolume: vol.export(0, 0, 0),
		Cells:            vol.count(),
		Radius:           smokeRadius,
	}, nil
}

// SightlinePair is one eye position to another. Both ends are eye positions,
// not feet: this endpoint answers "is this line blocked", and knows nothing
// about who is standing where. Stance belongs to OneWay.
type SightlinePair struct {
	From Point `json:"from"`
	To   Point `json:"to"`
}

// CloudSpec is the smoke a request is asking about — either a point to bloom
// from, or a grid already computed (typically one /smoke-volume handed back).
// Supplying the grid skips the flood, which is the expensive half.
type CloudSpec struct {
	At *Point `json:"at,omitempty"`
	// Volume is an EventSmokeVolume as exported by /smoke-volume or carried in
	// a playback blob. Its density is quantised to 16 levels, so depths through
	// a round-tripped grid differ from the source by up to one level per cell.
	Volume *EventSmokeVolume `json:"volume,omitempty"`
}

// SightlineRequest asks, for each pair, whether smoke and geometry block it.
type SightlineRequest struct {
	Map string `json:"map"`
	// Smokes is the full form. Smoke is the one-cloud shorthand, and At is the
	// shorthand for that shorthand, since most callers have a detonation point
	// and nothing else.
	Smokes []CloudSpec     `json:"smokes,omitempty"`
	Smoke  *CloudSpec      `json:"smoke,omitempty"`
	At     *Point          `json:"at,omitempty"`
	Pairs  []SightlinePair `json:"pairs"`
	// Threshold is the optical depth at which a line counts as blocked;
	// DefaultBlockThreshold when nil or non-positive.
	Threshold *float64 `json:"threshold,omitempty"`
}

func (r SightlineRequest) clouds() []CloudSpec {
	specs := append([]CloudSpec(nil), r.Smokes...)
	if r.Smoke != nil {
		specs = append(specs, *r.Smoke)
	}
	if r.At != nil {
		specs = append(specs, CloudSpec{At: r.At})
	}
	return specs
}

func (r SightlineRequest) threshold() float64 {
	if r.Threshold == nil || *r.Threshold <= 0 {
		return DefaultBlockThreshold
	}
	return *r.Threshold
}

// SightlineResult is one pair's answer.
type SightlineResult struct {
	// Blocked is the headline: the far end could not be made out from the near
	// end, whether the map or the smoke did it.
	Blocked bool `json:"blocked"`
	// BlockedBy is "world" when the map alone blocks the line (the smoke is
	// irrelevant to it), "smoke" when the clouds do, and empty when it is open.
	// A line the map already blocks is never attributed to smoke, so a lineup
	// cannot take credit for a wall.
	BlockedBy    string `json:"blocked_by,omitempty"`
	WorldBlocked bool   `json:"world_blocked"`
	// Depth is the smoke on the line, in cell widths of fully dense smoke, and
	// Transmittance is e^-Depth: the fraction of the target's contrast that
	// survives. Reported whatever the threshold, so a caller can re-cut the
	// answer without another request.
	Depth         float64 `json:"depth"`
	Transmittance float64 `json:"transmittance"`
	// PerSmoke splits Depth across the request's clouds, in the order they were
	// resolved. Lets a caller see which smoke of a set is doing the work.
	PerSmoke []float64 `json:"per_smoke,omitempty"`
	Distance float64   `json:"distance"`
}

// CloudInfo describes a cloud as the service resolved it, so a caller can tell
// what was actually measured — particularly when a point failed to bloom.
type CloudInfo struct {
	Center Point `json:"center"`
	// Model is "voxel" for a flooded or supplied grid and "sphere" for the
	// fallback used when a point seals. A sphere ignores the map, so an answer
	// resting on one is a guess; it is named rather than hidden.
	Model  string  `json:"model"`
	Cells  int     `json:"cells,omitempty"`
	Radius float64 `json:"radius"`
	Sealed bool    `json:"sealed,omitempty"`
}

type SightlineResponse struct {
	Map       string            `json:"map"`
	Threshold float64           `json:"threshold"`
	Smokes    []CloudInfo       `json:"smokes"`
	Results   []SightlineResult `json:"results"`
}

// Sightlines answers a batch of point-to-point visibility questions against a
// map and a set of clouds.
func Sightlines(mesh *geometry.Mesh, req SightlineRequest) (SightlineResponse, error) {
	if mesh == nil {
		return SightlineResponse{}, ErrNoMesh
	}
	if len(req.Pairs) == 0 {
		return SightlineResponse{}, errors.New("pairs must not be empty")
	}
	if len(req.Pairs) > maxSightlinePairs {
		return SightlineResponse{}, fmt.Errorf("too many pairs: %d (max %d)", len(req.Pairs), maxSightlinePairs)
	}
	clouds, infos, err := resolveClouds(mesh, req.clouds())
	if err != nil {
		return SightlineResponse{}, err
	}

	threshold := req.threshold()
	out := SightlineResponse{Map: req.Map, Threshold: threshold, Smokes: infos}
	out.Results = make([]SightlineResult, 0, len(req.Pairs))
	for i, pair := range req.Pairs {
		if !pair.From.valid() || !pair.To.valid() {
			return SightlineResponse{}, fmt.Errorf("pair %d: coordinates must be finite", i)
		}
		out.Results = append(out.Results, sightline(mesh, clouds, pair.From.vec(), pair.To.vec(), threshold))
	}
	return out, nil
}

func sightline(mesh *geometry.Mesh, clouds []resolvedCloud, from, to r3.Vector, threshold float64) SightlineResult {
	res := SightlineResult{
		WorldBlocked: mesh.Occluded(from, to),
		Distance:     from.Sub(to).Norm(),
	}
	if len(clouds) > 0 {
		res.PerSmoke = make([]float64, len(clouds))
		for i, c := range clouds {
			d := c.depth(from, to)
			res.PerSmoke[i] = d
			res.Depth += d
		}
	}
	res.Transmittance = math.Exp(-res.Depth)
	switch {
	case res.WorldBlocked:
		res.Blocked, res.BlockedBy = true, "world"
	case res.Depth >= threshold:
		res.Blocked, res.BlockedBy = true, "smoke"
	}
	return res
}

// resolvedCloud is one cloud ready to be integrated along a segment.
type resolvedCloud struct {
	// vol is nil for the sphere fallback.
	vol    *smokeVolume
	center r3.Vector
	// radius gates the voxel walk (cells further than this from center are not
	// counted, which is how the parser models a cloud still billowing out) and
	// is the sphere's radius in the fallback. For a settled cloud it is set
	// wide enough to include every cell.
	radius float64
}

// depth is how much smoke this cloud puts on a segment, in cell widths of full
// density.
func (c resolvedCloud) depth(from, to r3.Vector) float64 {
	if c.vol != nil {
		return c.vol.opticalDepth(from, to, c.center, c.radius, nil)
	}
	// Sphere fallback: a chord through a uniformly dense ball. Expressed in the
	// same cell-width units so a threshold means the same thing either way.
	return sphereChord(from, to, c.center, c.radius) / smokeVoxelSize
}

func resolveClouds(mesh *geometry.Mesh, specs []CloudSpec) ([]resolvedCloud, []CloudInfo, error) {
	if len(specs) > maxRequestClouds {
		return nil, nil, fmt.Errorf("too many smokes: %d (max %d)", len(specs), maxRequestClouds)
	}
	clouds := make([]resolvedCloud, 0, len(specs))
	infos := make([]CloudInfo, 0, len(specs))
	for i, spec := range specs {
		switch {
		case spec.Volume != nil:
			vol, err := volumeFromExport(*spec.Volume)
			if err != nil {
				return nil, nil, fmt.Errorf("smoke %d: %w", i, err)
			}
			center, radius := vol.boundingSphere()
			clouds = append(clouds, resolvedCloud{vol: vol, center: center, radius: radius})
			infos = append(infos, CloudInfo{
				Center: Point{X: center.X, Y: center.Y, Z: center.Z},
				Model:  "voxel",
				Cells:  vol.count(),
				Radius: radius,
			})
		case spec.At != nil:
			at := *spec.At
			if !at.valid() {
				return nil, nil, fmt.Errorf("smoke %d: coordinates must be finite", i)
			}
			center := at.vec()
			info := CloudInfo{Center: at, Model: "voxel", Radius: smokeRadius}
			vol, sealed := buildSmokeVolume(mesh, center)
			if sealed || vol == nil {
				// Answering with nothing would report every sightline through
				// the cloud as open, which is a worse lie than a sphere.
				info.Model, info.Sealed = "sphere", true
				clouds = append(clouds, resolvedCloud{center: center, radius: smokeRadius})
				infos = append(infos, info)
				continue
			}
			// The cloud is settled, so nothing should be trimmed by the bloom
			// gate: reach past the far corner of the grid.
			_, radius := vol.boundingSphere()
			clouds = append(clouds, resolvedCloud{vol: vol, center: center, radius: math.Max(radius, smokeRadius)})
			info.Cells = vol.count()
			infos = append(infos, info)
		default:
			return nil, nil, fmt.Errorf("smoke %d: needs either at or volume", i)
		}
	}
	return clouds, infos, nil
}

// boundingSphere returns the centre of the grid and a radius that reaches every
// cell in it, so a settled cloud is never trimmed by the bloom gate.
func (v *smokeVolume) boundingSphere() (r3.Vector, float64) {
	half := r3.Vector{
		X: float64(v.dim[0]) * v.size / 2,
		Y: float64(v.dim[1]) * v.size / 2,
		Z: float64(v.dim[2]) * v.size / 2,
	}
	center := r3.Vector{
		X: v.origin.X + half.X,
		Y: v.origin.Y + half.Y,
		Z: v.origin.Z + half.Z,
	}
	return center, half.Norm()
}

// volumeFromExport rebuilds a density field from the wire form export produces.
// Quantisation is not undone — a cell that left as one of 16 levels comes back
// as the midpoint of that level — so a round-tripped grid gives depths within
// about one level per cell of the original.
func volumeFromExport(ex EventSmokeVolume) (*smokeVolume, error) {
	if ex.DimX <= 0 || ex.DimY <= 0 || ex.DimZ <= 0 {
		return nil, errors.New("volume dims must be positive")
	}
	if ex.VoxelSize <= 0 {
		return nil, errors.New("volume voxel size must be positive")
	}
	total := ex.DimX * ex.DimY * ex.DimZ
	if total > maxSuppliedCells {
		return nil, fmt.Errorf("volume is %d cells (max %d)", total, maxSuppliedCells)
	}
	packed, err := base64.StdEncoding.DecodeString(ex.Density)
	if err != nil {
		return nil, fmt.Errorf("volume density is not valid base64: %w", err)
	}
	if len(packed) != (total+1)/2 {
		return nil, fmt.Errorf("volume density is %d bytes, want %d for %d cells",
			len(packed), (total+1)/2, total)
	}
	v := &smokeVolume{
		origin:  r3.Vector{X: float64(ex.OriginX), Y: float64(ex.OriginY), Z: float64(ex.OriginZ)},
		size:    float64(ex.VoxelSize),
		dim:     [3]int{ex.DimX, ex.DimY, ex.DimZ},
		density: make([]uint8, total),
	}
	for n := 0; n < total; n++ {
		q := packed[n>>1] & 0x0f
		if n&1 == 1 {
			q = packed[n>>1] >> 4
		}
		if q == 0 {
			continue
		}
		// export rounds a cell up into its level (level = d*15/max + 1), so the
		// densities that produced a given level span [(q-1), q) levels. Decode
		// to the middle of that span rather than its top, or every cell comes
		// back heavier than it went in and long chords drift measurably.
		v.density[n] = uint8(int(q)*densityMax/15 - densityMax/30)
	}
	return v, nil
}

// sphereChord is the length of the part of a segment that lies inside a sphere.
func sphereChord(from, to, center r3.Vector, radius float64) float64 {
	d := to.Sub(from)
	segLen := d.Norm()
	if segLen < 1e-9 || radius <= 0 {
		return 0
	}
	m := from.Sub(center)
	// |m + t*d|² = r², solved for the parametric range inside the sphere.
	a := d.Dot(d)
	b := 2 * m.Dot(d)
	c := m.Dot(m) - radius*radius
	disc := b*b - 4*a*c
	if disc <= 0 {
		return 0
	}
	sq := math.Sqrt(disc)
	t0 := math.Max((-b-sq)/(2*a), 0)
	t1 := math.Min((-b+sq)/(2*a), 1)
	if t1 <= t0 {
		return 0
	}
	return (t1 - t0) * segLen
}
