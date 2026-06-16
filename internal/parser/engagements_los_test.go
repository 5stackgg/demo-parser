package parser

import (
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// wallTriBlob serializes a single wall quad (two triangles in the X=0 plane,
// y,z ∈ [-50,50]) into the .tri wire format (9 LE float32 per triangle).
func wallTriBlob() []byte {
	a := r3.Vector{X: 0, Y: -50, Z: -50}
	b := r3.Vector{X: 0, Y: 50, Z: -50}
	c := r3.Vector{X: 0, Y: 50, Z: 50}
	d := r3.Vector{X: 0, Y: -50, Z: 50}
	tris := [][3]r3.Vector{{a, b, c}, {a, c, d}}
	buf := make([]byte, 0, len(tris)*9*4)
	put := func(f float64) {
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], math.Float32bits(float32(f)))
		buf = append(buf, p[:]...)
	}
	for _, tr := range tris {
		for _, v := range tr {
			put(v.X)
			put(v.Y)
			put(v.Z)
		}
	}
	return buf
}

func TestLosGating(t *testing.T) {
	// Two eye points on opposite sides of the X=0 wall: with no mesh the
	// sightline is treated as clear (visible), with the wall mesh it's
	// occluded — the difference is what gates a spot/engagement.
	from := r3.Vector{X: -100, Y: 0, Z: 0}
	to := r3.Vector{X: 100, Y: 0, Z: 0}

	tests := []struct {
		name    string
		withMap bool
		want    bool
	}{
		{name: "nil mesh treated visible", withMap: false, want: true},
		{name: "wall mesh occludes", withMap: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &state{res: &Result{}, meshTried: true}
			if tc.withMap {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(wallTriBlob())
				}))
				defer srv.Close()
				t.Setenv("MAP_MESH_CDN", srv.URL)
				s.res.MapName = "los_test_map"
				s.meshTried = false
				mesh, err := geometry.Load(s.res.MapName)
				if err != nil {
					t.Fatalf("load mesh: %v", err)
				}
				if mesh == nil {
					t.Fatal("expected a wall mesh to load")
				}
				s.mesh = mesh
				s.meshTried = true
			}
			if got := s.los(from, to); got != tc.want {
				t.Fatalf("los = %v, want %v", got, tc.want)
			}
		})
	}
}
