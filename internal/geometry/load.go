package geometry

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/geo/r3"
)

// defaultMeshCDN matches the pinned revision the web 3D replay uses
// (web/nuxt.config.ts public.mapMeshCdn). Override via MAP_MESH_CDN; set it
// empty to disable geometry entirely (offline / tests).
const defaultMeshCDN = "https://cdn.jsdelivr.net/gh/5stackgg/replay-map-meshes@17595823-4"

// maxMeshBytes caps a downloaded .tri, matching the web's MAX_MESH_BYTES.
const maxMeshBytes = 96 << 20

// maxTriangles caps how big a mesh we keep. A .tri over this budget is
// treated as no mesh rather than cached forever (los then falls back to
// "always visible").
const maxTriangles = 1_500_000

var client = &http.Client{Timeout: 15 * time.Second}

// cached memoizes one Load attempt per normalized map name (including the
// "no geometry" result, so a missing .tri is fetched at most once).
type cached struct {
	once sync.Once
	mesh *Mesh
	err  error
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*cached{}
)

// normalizeMapName turns a parser map name into a .tri base name: workshop
// maps (`workshop/123/de_x`) → `de_x`, lowercased, `_night` stripped.
func normalizeMapName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	n = strings.TrimSuffix(n, "_night")
	return n
}

func cdnBase() (string, bool) {
	if v, ok := os.LookupEnv("MAP_MESH_CDN"); ok {
		return strings.TrimRight(v, "/"), true
	}
	return defaultMeshCDN, true
}

// Load returns the collision mesh for a map, or (nil, nil) when geometry is
// unavailable (disabled, unknown map, or no .tri published) — callers treat a
// nil mesh as "always visible". Results are cached process-wide.
func Load(mapName string) (*Mesh, error) {
	key := normalizeMapName(mapName)
	if key == "" {
		return nil, nil
	}
	cacheMu.Lock()
	c := cache[key]
	if c == nil {
		c = &cached{}
		cache[key] = c
	}
	cacheMu.Unlock()
	c.once.Do(func() { c.mesh, c.err = fetchAndBuild(key) })
	return c.mesh, c.err
}

func fetchAndBuild(key string) (*Mesh, error) {
	base, _ := cdnBase()
	if base == "" {
		return nil, nil // geometry disabled
	}
	resp, err := client.Get(base + "/" + key + ".tri")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no mesh published for this map
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh %s: status %d", key, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMeshBytes))
	if err != nil {
		return nil, err
	}
	return buildMesh(data), nil
}

// buildMesh parses a raw .tri blob (little-endian float32, 9 floats per
// triangle: 3 vertices × xyz, source units, Z = height) and builds the BVH.
func buildMesh(data []byte) *Mesh {
	nTri := (len(data) / 4) / 9
	if nTri == 0 || nTri > maxTriangles {
		return nil
	}
	f := func(o int) float64 {
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(data[o:])))
	}
	tris := make([]triangle, 0, nTri)
	for i := 0; i < nTri; i++ {
		o := i * 9 * 4
		a := r3.Vector{X: f(o), Y: f(o + 4), Z: f(o + 8)}
		b := r3.Vector{X: f(o + 12), Y: f(o + 16), Z: f(o + 20)}
		c := r3.Vector{X: f(o + 24), Y: f(o + 28), Z: f(o + 32)}
		tris = append(tris, newTriangle(a, b, c))
	}
	m := &Mesh{tris: tris}
	m.build()
	return m
}
