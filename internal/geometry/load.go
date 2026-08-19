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

// The mesh sets are published as tagged snapshots of one GitHub repo and
// served through jsDelivr, so a revision is fully identified by its tag. These
// let a caller name a revision other than the process default — which is what
// drift detection needs, since it has to hold the mesh from before a map patch
// and the one from after it at the same time.
const (
	defaultMeshOwner = "5stackgg"
	defaultMeshRepo  = "5stackgg/replay-map-meshes"
)

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
		// The key is base + "\n" + map; only the map half is worth logging.
		_, name, _ := strings.Cut(evict, "\n")
		fmt.Fprintf(os.Stderr, "[geometry] evicted mesh for %s (%d cached)\n", name, len(lru))
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

// MaxCachedMeshes reports the bound the mesh cache is running under, so a
// caller that needs more than one mesh resident at a time (drift detection
// holds two) can warn when the deployment is configured for fewer.
func MaxCachedMeshes() int {
	return maxCachedMeshes()
}

// ResolveMeshRevision turns a mesh reference into the base URL its .tri files
// live under. Accepted forms, most to least specific:
//
//	"https://host/path"                       used verbatim (mirrors, tests)
//	"5stackgg/replay-map-meshes@17595823-5"   owner, repo and tag
//	"replay-map-meshes@17595823-5"            default owner
//	"17595823-5"                              default owner and repo
//	""                                        the process default (MAP_MESH_CDN)
//
// A reference is part of a request body, so the repo and tag are charset-
// checked rather than pasted into a URL: a "tag" containing a slash or a dot
// segment would otherwise walk out of the pinned path and fetch something else
// entirely.
func ResolveMeshRevision(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		base, _ := cdnBase()
		return strings.TrimRight(base, "/"), nil
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return strings.TrimRight(ref, "/"), nil
	}
	repo, tag := defaultMeshRepo, ref
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		repo, tag = ref[:i], ref[i+1:]
		if !strings.Contains(repo, "/") {
			repo = defaultMeshOwner + "/" + repo
		}
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !safeRefPart(owner) || !safeRefPart(name) || !safeRefPart(tag) {
		return "", fmt.Errorf("mesh revision %q is not a tag, owner/repo@tag, or an http(s) base", ref)
	}
	return "https://cdn.jsdelivr.net/gh/" + owner + "/" + name + "@" + tag, nil
}

func safeRefPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// LoadRevision is Load against a named mesh revision rather than the process
// default. Two revisions of the same map are separate cache entries, so both
// can be resident at once — mind MaxCachedMeshes when they are.
func LoadRevision(mapName, revision string) (*Mesh, error) {
	base, err := ResolveMeshRevision(revision)
	if err != nil {
		return nil, err
	}
	return loadFrom(base, mapName)
}

// Load returns the collision mesh for a map, or (nil, nil) when geometry is
// unavailable (disabled, unknown map, or no .tri published) — callers treat a
// nil mesh as "always visible". Results are cached process-wide, bounded to
// maxCachedMeshes built meshes.
func Load(mapName string) (*Mesh, error) {
	base, _ := cdnBase()
	return loadFrom(strings.TrimRight(base, "/"), mapName)
}

// loadFrom is Load against an already-resolved base. The cache key carries the
// base as well as the map, so the same map at two revisions never collides —
// the whole point of drift detection is that those two meshes differ.
func loadFrom(base, mapName string) (*Mesh, error) {
	name := normalizeMapName(mapName)
	if name == "" {
		return nil, nil
	}
	key := base + "\n" + name
	cacheMu.Lock()
	c := cache[key]
	if c == nil {
		c = &cached{}
		cache[key] = c
	}
	cacheMu.Unlock()
	c.once.Do(func() { c.mesh, c.err = fetchAndBuild(base, name) })

	cacheMu.Lock()
	switch {
	case c.err != nil:
		// Drop failed attempts so the next parse retries. A memoized error is a
		// sync.Once that can never run again: one CDN timeout would otherwise
		// disable sightline validation for that map until the pod restarts.
		// A 404 is not an error — that returns (nil, nil) and stays memoized.
		if cache[key] == c {
			delete(cache, key)
		}
	case c.mesh != nil:
		// Registered after the build so a mesh can never evict itself while it
		// is the one being loaded, and so absent meshes never displace a real
		// one. An entry evicted between the lookup above and here is simply
		// built again next time — the caller still gets a valid mesh.
		if cache[key] == c {
			touch(key)
		}
	}
	cacheMu.Unlock()
	return c.mesh, c.err
}

func fetchAndBuild(base, key string) (*Mesh, error) {
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
