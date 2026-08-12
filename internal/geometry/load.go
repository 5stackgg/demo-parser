package geometry

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
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

// defaultMaxCachedMeshes bounds how many built meshes stay resident. A mesh is
// tens of MB and a parse only ever touches the one map it is parsing, so a
// small LRU keeps steady-state memory flat no matter how many maps pass
// through — workshop maps in particular, since normalizeMapName turns every
// variant into its own key and nothing else would ever release them. A miss
// costs one re-download plus a rebuild, seconds against a 20-30s parse.
// Override with MAP_MESH_CACHE; 0 or less disables caching entirely.
const defaultMaxCachedMeshes = 2

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
	// Keys of entries holding a non-nil mesh, most recently used first. Only
	// those count against the bound: a map with no .tri memoizes as (nil, nil)
	// and costs nothing, so those entries stay forever and a missing mesh is
	// still fetched at most once.
	lru []string
)

func maxCachedMeshes() int {
	if v, ok := os.LookupEnv("MAP_MESH_CACHE"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return defaultMaxCachedMeshes
}

// touch moves key to the front of the LRU and evicts the least recently used
// meshes past the bound. Callers must hold cacheMu.
//
// Eviction is only a map delete: a parse already holding the *Mesh keeps it
// alive for as long as it needs it, and the GC reclaims it once that parse
// finishes. Nothing here can pull a mesh out from under an in-flight parse.
func touch(key string) {
	for i, k := range lru {
		if k == key {
			lru = append(lru[:i], lru[i+1:]...)
			break
		}
	}
	lru = append([]string{key}, lru...)

	max := maxCachedMeshes()
	if max < 0 {
		max = 0
	}
	for len(lru) > max {
		evict := lru[len(lru)-1]
		lru = lru[:len(lru)-1]
		delete(cache, evict)
		fmt.Fprintf(os.Stderr, "[geometry] evicted mesh for %s (%d cached)\n", evict, len(lru))
	}
}

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
// nil mesh as "always visible". Results are cached process-wide, bounded to
// maxCachedMeshes built meshes.
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

	// Registered after the build so a mesh can never evict itself while it is
	// the one being loaded, and so failed/absent meshes never displace a real
	// one. An entry evicted between the lookup above and here is simply built
	// again next time — the caller still gets a valid mesh.
	if c.mesh != nil {
		cacheMu.Lock()
		if cache[key] == c {
			touch(key)
		}
		cacheMu.Unlock()
	}
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
