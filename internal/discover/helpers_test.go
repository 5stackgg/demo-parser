package discover

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

type tri = [3]r3.Vector

func quad(a, b, c, d r3.Vector) []tri {
	return []tri{{a, b, c}, {a, c, d}}
}

func rect(x0, x1, y0, y1, z float64) []tri {
	return quad(v(x0, y0, z), v(x1, y0, z), v(x1, y1, z), v(x0, y1, z))
}

func wallX(x, y0, y1, z0, z1 float64) []tri {
	return quad(v(x, y0, z0), v(x, y1, z0), v(x, y1, z1), v(x, y0, z1))
}

func gzipped(raw []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return out.Bytes()
}

// synthWorld serves triangles as map "de_test" through the real loader.
func synthWorld(t testing.TB, tris []tri) *geometry.GrenadeWorld {
	t.Helper()
	raw := make([]byte, 0, len(tris)*36)
	for _, tr := range tris {
		for _, p := range tr {
			for _, f := range [3]float64{p.X, p.Y, p.Z} {
				raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(float32(f)))
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) != "de_test.tri.gz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(gzipped(raw))
	}))
	t.Cleanup(srv.Close)
	world, err := geometry.LoadGrenadeWorld("de_test", srv.URL)
	if err != nil || world == nil {
		t.Fatalf("load synthetic world: %v", err)
	}
	return world
}

// mirageWorld is the real de_mirage, from a local replay-map-meshes clone
// (MAP_MESH_FIXTURES) or, with DISCOVER_LIVE_MESH=1, the published build. The
// recorded throws only mean anything against the map they were thrown on.
func mirageWorld(t testing.TB) *geometry.GrenadeWorld {
	t.Helper()
	dir := os.Getenv("MAP_MESH_FIXTURES")
	if dir == "" {
		if os.Getenv("DISCOVER_LIVE_MESH") == "" {
			t.Skip("needs de_mirage: set MAP_MESH_FIXTURES to a replay-map-meshes clone, or DISCOVER_LIVE_MESH=1")
		}
		world, err := geometry.LoadGrenadeWorld("de_mirage", "")
		if err != nil || world == nil {
			t.Fatalf("load de_mirage: %v", err)
		}
		return world
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(filepath.Base(r.URL.Path), ".gz")))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(gzipped(raw))
	}))
	t.Cleanup(srv.Close)
	world, err := geometry.LoadGrenadeWorld("de_mirage", srv.URL)
	if err != nil || world == nil {
		t.Fatalf("load de_mirage: %v", err)
	}
	return world
}
