package simulate

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// tri is one triangle's three corners, and quad is the two triangles of a
// rectangle — the only two shapes the synthetic meshes here are built from.
type tri = [3]r3.Vector

func quad(a, b, c, d r3.Vector) []tri {
	return []tri{{a, b, c}, {a, c, d}}
}

// floorQuad is a horizontal square at height z, centred on (cx, cy).
func floorQuad(cx, cy, z, half float64) []tri {
	return quad(
		r3.Vector{X: cx - half, Y: cy - half, Z: z},
		r3.Vector{X: cx + half, Y: cy - half, Z: z},
		r3.Vector{X: cx + half, Y: cy + half, Z: z},
		r3.Vector{X: cx - half, Y: cy + half, Z: z},
	)
}

// box is the six faces of an axis-aligned box, which is how a test builds
// either an obstacle to bounce off or a sealed pocket to be buried in.
func box(lo, hi r3.Vector) []tri {
	p := [8]r3.Vector{
		{X: lo.X, Y: lo.Y, Z: lo.Z}, {X: hi.X, Y: lo.Y, Z: lo.Z},
		{X: hi.X, Y: hi.Y, Z: lo.Z}, {X: lo.X, Y: hi.Y, Z: lo.Z},
		{X: lo.X, Y: lo.Y, Z: hi.Z}, {X: hi.X, Y: lo.Y, Z: hi.Z},
		{X: hi.X, Y: hi.Y, Z: hi.Z}, {X: lo.X, Y: hi.Y, Z: hi.Z},
	}
	var out []tri
	out = append(out, quad(p[0], p[1], p[2], p[3])...) // bottom
	out = append(out, quad(p[4], p[5], p[6], p[7])...) // top
	out = append(out, quad(p[0], p[1], p[5], p[4])...) // -Y
	out = append(out, quad(p[3], p[2], p[6], p[7])...) // +Y
	out = append(out, quad(p[0], p[3], p[7], p[4])...) // -X
	out = append(out, quad(p[1], p[2], p[6], p[5])...) // +X
	return out
}

// triBlob serializes triangles into the .tri wire format (9 LE float32 each),
// so a synthetic mesh goes through exactly the loader a shipped one does.
func triBlob(tris ...tri) []byte {
	buf := make([]byte, 0, len(tris)*9*4)
	put := func(f float64) {
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], math.Float32bits(float32(f)))
		buf = append(buf, p[:]...)
	}
	for _, t := range tris {
		for _, v := range t {
			put(v.X)
			put(v.Y)
			put(v.Z)
		}
	}
	return buf
}

// meshRevisionServer stands in for one tagged mesh revision on the CDN, serving
// the named .tri blobs. It returns a base URL usable as a mesh reference, which
// is how a test gets two independently built meshes resident at once.
func meshRevisionServer(t testing.TB, files map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blob, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(blob)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func loadMesh(t testing.TB, base, name string) *geometry.Mesh {
	t.Helper()
	mesh, err := geometry.LoadRevision(name, base)
	if err != nil {
		t.Fatalf("load %s from %s: %v", name, base, err)
	}
	if mesh == nil {
		t.Fatalf("no mesh for %s at %s", name, base)
	}
	return mesh
}

// synthRevision publishes one map ("de_test") built from the given triangles
// and returns both the mesh and the revision reference that produced it.
func synthRevision(t testing.TB, tris []tri) (*geometry.Mesh, string) {
	t.Helper()
	base := meshRevisionServer(t, map[string][]byte{"de_test.tri": triBlob(tris...)})
	return loadMesh(t, base, "de_test"), base
}

// localMeshDir finds the replay-map-meshes clone checked out above this repo,
// the same way the endpoint tests do. Real geometry rather than a box: a
// bounce off a shipped mesh crosses triangle seams, which is exactly where a
// sloppy simulator would stop being reproducible.
func localMeshDir(t testing.TB) string {
	t.Helper()
	if dir := os.Getenv("MAP_MESH_FIXTURES"); dir != "" {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; ; {
		candidate := filepath.Join(d, "replay-map-meshes")
		if _, err := os.Stat(filepath.Join(candidate, "de_mirage.tri")); err == nil {
			return candidate
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// realMeshRevision serves the local clone as a mesh revision. Each call is a
// separate server, so asking twice yields two independently built meshes of
// identical geometry — the strongest form of the same-mesh property.
func realMeshRevision(t testing.TB) string {
	t.Helper()
	dir := localMeshDir(t)
	if dir == "" {
		t.Skip("no replay-map-meshes clone found above the working directory; set MAP_MESH_FIXTURES")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv.URL
}

func pt(x, y, z float64) r3.Vector { return r3.Vector{X: x, Y: y, Z: z} }

// rectQuad is an axis-aligned horizontal rectangle, for building floors with
// pieces missing.
func rectQuad(x0, x1, y0, y1, z float64) []tri {
	return quad(
		r3.Vector{X: x0, Y: y0, Z: z},
		r3.Vector{X: x1, Y: y0, Z: z},
		r3.Vector{X: x1, Y: y1, Z: z},
		r3.Vector{X: x0, Y: y1, Z: z},
	)
}

// terrain is the synthetic map the drift tests throw across: open ground, a
// back wall to bounce off and a ramp to roll down, so a random throw has
// somewhere interesting to end up rather than always landing on a flat plane.
func terrain() []tri {
	out := floorQuad(0, 0, 0, 2000)
	out = append(out, quad(
		r3.Vector{X: 1800, Y: -2000, Z: 0}, r3.Vector{X: 1800, Y: 2000, Z: 0},
		r3.Vector{X: 1800, Y: 2000, Z: 400}, r3.Vector{X: 1800, Y: -2000, Z: 400})...)
	out = append(out, quad(
		r3.Vector{X: -1200, Y: -600, Z: 0}, r3.Vector{X: -600, Y: -600, Z: 240},
		r3.Vector{X: -600, Y: 600, Z: 240}, r3.Vector{X: -1200, Y: 600, Z: 0})...)
	return out
}

// floorWithHole is terrain's floor with a rectangle missing at
// x∈[800,1600], y∈[-400,400] — exactly the patch floorQuad(1200, 0, 0, 400)
// covers, so the two meshes differ by that one piece and nothing else.
func floorWithHole() []tri {
	out := rectQuad(-2000, 800, -2000, 2000, 0)
	out = append(out, rectQuad(1600, 2000, -2000, 2000, 0)...)
	out = append(out, rectQuad(800, 1600, -2000, -400, 0)...)
	out = append(out, rectQuad(800, 1600, 400, 2000, 0)...)
	return out
}

// meshPair is a built mesh together with the revision reference that produced
// it, which is what a DriftRequest names.
type meshPair struct {
	mesh *geometry.Mesh
	ref  string
}

func newMeshPair(t testing.TB, tris []tri) *meshPair {
	t.Helper()
	mesh, ref := synthRevision(t, tris)
	return &meshPair{mesh: mesh, ref: ref}
}

// randomConstants walks the whole knob space, well past anything plausible.
// The same-mesh property has to hold for a model that is wrong in any way at
// all, so the test does not get to assume the constants are sensible.
func randomConstants(rng *rand.Rand) Constants {
	pick := func(lo, hi float64) float64 { return lo + rng.Float64()*(hi-lo) }
	return Constants{
		TimeStep:           1 / pick(48, 300),
		Gravity:            pick(100, 800),
		Radius:             pick(0, 8),
		Restitution:        pick(0, 0.95),
		Friction:           pick(0, 0.9),
		ContactNormalSpeed: pick(0, 80),
		RollingFriction:    pick(0, 12),
		FloorNormalZ:       pick(0.2, 0.95),
		RestSpeed:          pick(2, 60),
		RestSteps:          1 + rng.Intn(8),
		MaxFlightSeconds:   pick(8, 20),
		HEFuseSeconds:      pick(0.3, 3),
		FlashFuseSeconds:   pick(0.3, 3),
		MolotovArmSeconds:  pick(0, 1),
		EnclosureProbe:     pick(0, 24),
		WorldMargin:        pick(64, 2048),
	}
}

// overridesFrom turns a full constant set into the override form a request
// carries, so a test can push an arbitrary model through the request path.
func overridesFrom(c Constants) *ConstantOverrides {
	return &ConstantOverrides{
		TimeStep: &c.TimeStep, Gravity: &c.Gravity, Radius: &c.Radius,
		Restitution: &c.Restitution, Friction: &c.Friction,
		ContactNormalSpeed: &c.ContactNormalSpeed, RollingFriction: &c.RollingFriction,
		FloorNormalZ: &c.FloorNormalZ, RestSpeed: &c.RestSpeed, RestSteps: &c.RestSteps,
		MaxFlightSeconds: &c.MaxFlightSeconds, HEFuseSeconds: &c.HEFuseSeconds,
		FlashFuseSeconds: &c.FlashFuseSeconds, MolotovArmSeconds: &c.MolotovArmSeconds,
		EnclosureProbe: &c.EnclosureProbe, WorldMargin: &c.WorldMargin,
	}
}

// randomLineups is a batch of throws in every direction, including a few with
// no recorded seed — the library is full of those and they have to survive the
// same code path.
func randomLineups(rng *rand.Rand, n int) []LineupSeed {
	types := []string{"Smoke", "HE", "Flash", "Molotov", "Decoy"}
	out := make([]LineupSeed, 0, n)
	for i := 0; i < n; i++ {
		l := LineupSeed{ID: fmt.Sprintf("lineup-%d", i), NadeType: types[rng.Intn(len(types))]}
		if i%7 != 3 {
			yaw := rng.Float64() * 2 * math.Pi
			speed := 100 + rng.Float64()*800
			l.InitialPosition = &Point{
				X: rng.Float64()*800 - 400,
				Y: rng.Float64()*800 - 400,
				Z: 32 + rng.Float64()*64,
			}
			l.InitialVelocity = &Point{
				X: speed * math.Cos(yaw),
				Y: speed * math.Sin(yaw),
				Z: rng.Float64()*500 - 150,
			}
		}
		out = append(out, l)
	}
	return out
}

func sameDrift(a, b LineupDrift) bool {
	if a.Index != b.Index || a.ID != b.ID || a.Verdict != b.Verdict ||
		a.Reason != b.Reason || a.Severity != b.Severity {
		return false
	}
	if !sameOutcome(a.From, b.From) || !sameOutcome(a.To, b.To) {
		return false
	}
	return sameFloat(a.Distance, b.Distance) &&
		sameFloat(a.DistanceXY, b.DistanceXY) &&
		sameFloat(a.DistanceZ, b.DistanceZ)
}

func sameOutcome(a, b *ComparableOutcome) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// terrainWithPlatform is terrain with the ground raised where the open throw
// used to land — a map update dropping a block under a lineup.
func terrainWithPlatform() []tri {
	return append(append([]tri(nil), terrain()...),
		box(pt(400, -1200, 0), pt(1799, 1200, 64))...)
}

// chamber is a walled room with a low front wall, so a nade lobbed in from
// outside clears the wall on the way in and is boxed in on every side once it
// settles. With the ceiling on, a landing inside has no space around it at all
// — which is what the enclosure probe is looking for.
//
// The ceiling sits at z=200 and the flight never rises past 130 inside the
// room, so adding it changes where the grenade ENDS UP being, and not one step
// of how it got there.
func chamber(withCeiling bool) []tri {
	const (
		x0, x1 = 0.0, 300.0
		y0, y1 = -150.0, 150.0
		top    = 200.0
		front  = 100.0
	)
	out := rectQuad(x0, x1, y0, y1, 0)
	out = append(out, quad(pt(x1, y0, 0), pt(x1, y1, 0), pt(x1, y1, top), pt(x1, y0, top))...)
	out = append(out, quad(pt(x0, y0, 0), pt(x1, y0, 0), pt(x1, y0, top), pt(x0, y0, top))...)
	out = append(out, quad(pt(x0, y1, 0), pt(x1, y1, 0), pt(x1, y1, top), pt(x0, y1, top))...)
	out = append(out, quad(pt(x0, y0, 0), pt(x0, y1, 0), pt(x0, y1, front), pt(x0, y0, front))...)
	if withCeiling {
		out = append(out, rectQuad(x0, x1, y0, y1, top)...)
	}
	return out
}
