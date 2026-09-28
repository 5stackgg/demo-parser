package geometry

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
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

// mapsHost is the root every published map asset hangs off: the panel's own
// Cloudflare worker (web/cloudflare-workers/backblaze-proxy) in front of B2.
// latest.json, every build's manifest.json and every manifest key are relative
// to it. A var only so tests can stand a server in for it.
var mapsHost = "https://demo-dl.5stack.gg/maps"

// pinnedBuild is the last build published before manifests existed. The
// default falls back to it when latest.json cannot be read, so an outage of the
// pointer degrades to older geometry rather than to none.
const pinnedBuild = "24957633"

const (
	latestTTL = 10 * time.Minute
	// A failed latest.json lookup is retried this soon. Every parse resolves
	// the default, so caching the failure at all is what keeps an outage from
	// costing each one a timeout.
	latestRetryTTL = 30 * time.Second
	maxIndexBytes  = 1 << 20
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

// asset is one per-map file a build publishes.
type asset int

const (
	hullAsset asset = iota
	grenadeClipAsset
)

func (a asset) legacySuffix() string {
	if a == grenadeClipAsset {
		return ".grenadeclip.tri.gz"
	}
	return ".tri.gz"
}

type manifestEntry struct {
	Tri         string `json:"tri"`
	GrenadeClip string `json:"grenadeclip"`
}

func (e manifestEntry) key(a asset) string {
	if a == grenadeClipAsset {
		return e.GrenadeClip
	}
	return e.Tri
}

// indexVersion is the only latest.json / manifest.json format this parser
// reads. An unknown version is treated like an unreachable pointer rather than
// guessed at.
const indexVersion = 1

type manifest struct {
	Version int                      `json:"version"`
	Build   string                   `json:"build"`
	Maps    map[string]manifestEntry `json:"maps"`
}

// source is where one revision's assets live. With a manifest, a map's files
// are wherever the manifest says under root — the publisher dedupes, so an
// unchanged map points into an older build's directory. Without one it is the
// flat legacy layout "<root>/<map><suffix>": MAP_MESH_CDN, an http(s)
// revision, and builds published before manifests existed.
type source struct {
	root     string
	manifest *manifest
	label    string
}

func legacySource(base string) source {
	base = strings.TrimRight(base, "/")
	return source{root: base, label: base}
}

// target is one asset's URL and whether a manifest named it. A file the
// manifest lists and the CDN then refuses is a publishing fault to retry, never
// "this map has none"; only a probe of a flat layout may come back absent.
type target struct {
	url    string
	listed bool
}

// A map the manifest does not list keeps the pinned build's flat hull, the
// same fallback the web viewer and the api callouts use, so the three never
// disagree about whether a map has geometry. It gets no grenade clips.
func (s source) target(name string, a asset) target {
	if s.root == "" {
		return target{}
	}
	if s.manifest == nil {
		return target{url: s.root + "/" + name + a.legacySuffix()}
	}
	if key := s.manifest.Maps[name].key(a); safeKey(key) {
		return target{url: s.root + "/" + key, listed: true}
	}
	if a == hullAsset {
		return target{url: s.root + "/" + pinnedBuild + "/" + name + a.legacySuffix()}
	}
	return target{}
}

type latestPointer struct {
	mu       sync.Mutex
	src      source
	resolved bool
	good     bool
	expires  time.Time
	inflight chan struct{}
}

var latest = &latestPointer{}

// get never holds the lock across the network: an expired pointer is served
// stale while one background refresh runs, and only a process that has never
// resolved one waits, on that same refresh. A failed refresh keeps the last
// good pointer; the pinned build is used only when latest.json has never been
// readable in this process.
func (l *latestPointer) get() source {
	l.mu.Lock()
	if l.resolved && time.Now().Before(l.expires) {
		defer l.mu.Unlock()
		return l.src
	}
	wait := l.inflight
	if wait == nil {
		wait = make(chan struct{})
		l.inflight = wait
		go l.refresh(mapsHost, wait)
	}
	if l.resolved {
		defer l.mu.Unlock()
		return l.src
	}
	l.mu.Unlock()
	<-wait
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.src
}

func (l *latestPointer) refresh(host string, done chan struct{}) {
	src, err := fetchLatest(host)
	l.mu.Lock()
	now := time.Now()
	if err != nil {
		l.expires = now.Add(latestRetryTTL)
		if !l.good {
			l.src = source{root: host + "/" + pinnedBuild, label: pinnedBuild}
		}
		fmt.Fprintf(os.Stderr, "[geometry] latest.json unavailable (%v); using build %s\n", err, l.src.label)
	} else {
		l.src, l.good, l.expires = src, true, now.Add(latestTTL)
	}
	l.resolved = true
	l.inflight = nil
	l.mu.Unlock()
	close(done)
}

// The manifest key is whatever latest.json names: a build republished with
// its failed maps rebuilt points at a revision such as <build>/manifest.r2.json.
func fetchLatest(host string) (source, error) {
	var pointer struct {
		Version  int    `json:"version"`
		Build    string `json:"build"`
		Manifest string `json:"manifest"`
	}
	found, err := getJSON(host+"/latest.json", &pointer)
	if err != nil {
		return source{}, err
	}
	if !found {
		return source{}, errors.New("latest.json is not published")
	}
	if pointer.Version != indexVersion {
		return source{}, fmt.Errorf("latest.json is version %d; this parser reads version %d", pointer.Version, indexVersion)
	}
	if !safeRefPart(pointer.Build) || !safeKey(pointer.Manifest) {
		return source{}, fmt.Errorf("latest.json names build %q, manifest %q", pointer.Build, pointer.Manifest)
	}
	m, found, err := loadManifest(host, pointer.Manifest)
	if err != nil {
		return source{}, err
	}
	if !found {
		return source{}, fmt.Errorf("%s is not published", pointer.Manifest)
	}
	return source{root: host, manifest: m, label: pointer.Build}, nil
}

// Only manifests that exist are cached: a published revision is immutable,
// while a "not found" can stop being true, and the keys come off request bodies.
var (
	manifestsMu sync.Mutex
	manifests   = map[string]*manifest{}
)

func loadManifest(host, key string) (*manifest, bool, error) {
	url := host + "/" + key
	manifestsMu.Lock()
	m := manifests[url]
	manifestsMu.Unlock()
	if m != nil {
		return m, true, nil
	}
	m = &manifest{}
	found, err := getJSON(url, m)
	if err != nil || !found {
		return nil, found, err
	}
	if m.Version != indexVersion {
		return nil, false, fmt.Errorf("%s is version %d; this parser reads version %d", key, m.Version, indexVersion)
	}
	if m.Maps == nil {
		return nil, false, fmt.Errorf("%s has no maps", key)
	}
	manifestsMu.Lock()
	manifests[url] = m
	manifestsMu.Unlock()
	return m, true, nil
}

// getJSON reports found=false for a 404 or 403: B2 has no ListBucket grant on
// these keys, so a genuinely missing object can arrive as either.
func getJSON(url string, into any) (bool, error) {
	resp, err := client.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIndexBytes)).Decode(into); err != nil {
		return false, fmt.Errorf("%s: %w", url, err)
	}
	return true, nil
}

func defaultSource() source {
	if v, ok := os.LookupEnv("MAP_MESH_CDN"); ok {
		return legacySource(v)
	}
	return latest.get()
}

// A build id names that build's first manifest revision, <build>/manifest.json.
func buildSource(build string) (source, error) {
	host := mapsHost
	m, found, err := loadManifest(host, build+"/manifest.json")
	if err != nil {
		return source{}, err
	}
	if !found {
		return source{root: host + "/" + build, label: build}, nil
	}
	return source{root: host, manifest: m, label: build}, nil
}

func isHTTPBase(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

func revisionSource(ref string) (source, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return defaultSource(), nil
	case isHTTPBase(ref):
		return legacySource(ref), nil
	case !safeRefPart(ref):
		return source{}, fmt.Errorf("mesh revision %q is not a build id or an http(s) base", ref)
	}
	return buildSource(ref)
}

// cached memoizes one Load attempt per asset URL (including the "no geometry"
// result, so a missing .tri is fetched at most once).
type cached struct {
	once sync.Once
	mesh *Mesh
	err  error
}

// meshCache is one bounded set of built meshes. Hulls and grenade clips are
// separate caches so the handful of clip meshes can never push a hull out:
// drift holds two of each at once.
type meshCache struct {
	kind    string
	mu      sync.Mutex
	entries map[string]*cached
	// Keys of entries holding a non-nil mesh, most recently used first. Only
	// those count against the bound: a map with no .tri memoizes as (nil, nil)
	// and costs nothing, so those entries stay forever and a missing mesh is
	// still fetched at most once.
	lru []string
}

var (
	hulls        = &meshCache{kind: "mesh", entries: map[string]*cached{}}
	grenadeClips = &meshCache{kind: "grenade clip", entries: map[string]*cached{}}
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
// meshes past the bound. Callers must hold c.mu.
//
// Eviction is only a map delete: a parse already holding the *Mesh keeps it
// alive for as long as it needs it, and the GC reclaims it once that parse
// finishes. Nothing here can pull a mesh out from under an in-flight parse.
func (c *meshCache) touch(key string) {
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}
	c.lru = append([]string{key}, c.lru...)

	max := maxCachedMeshes()
	if max < 0 {
		max = 0
	}
	for len(c.lru) > max {
		evict := c.lru[len(c.lru)-1]
		c.lru = c.lru[:len(c.lru)-1]
		delete(c.entries, evict)
		fmt.Fprintf(os.Stderr, "[geometry] evicted %s %s (%d cached)\n", c.kind, evict, len(c.lru))
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

// MaxCachedMeshes reports the bound the mesh cache is running under, so a
// caller that needs more than one mesh resident at a time (drift detection
// holds two) can warn when the deployment is configured for fewer.
func MaxCachedMeshes() int {
	return maxCachedMeshes()
}

// ResolveMeshRevision turns a mesh reference into the canonical spelling a
// report echoes, which loads the same meshes when handed back. Drift detection
// is what needs this: it holds the geometry from before a map patch and the
// geometry from after it at the same time, so a revision has to be nameable
// rather than just configured.
//
// Accepted forms, most to least specific:
//
//	"https://host/path"   a flat <map>.tri.gz directory, used verbatim (mirrors, tests)
//	"25537370"            a CS2 build: its manifest.json when it has one, else <build>/<map>.tri.gz
//	""                    the process default: MAP_MESH_CDN when set, else the build latest.json names
//
// A reference is part of a request body, so a build is charset-checked rather
// than pasted into a URL: one containing a slash or a dot segment would
// otherwise walk out of the maps root and fetch something else entirely.
func ResolveMeshRevision(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return defaultSource().label, nil
	case isHTTPBase(ref):
		return strings.TrimRight(ref, "/"), nil
	case !safeRefPart(ref):
		return "", fmt.Errorf("mesh revision %q is not a build id or an http(s) base", ref)
	}
	return ref, nil
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

// safeKey vets a manifest key, which is joined onto mapsHost: every segment
// must be a plain name, so no key can climb out of the maps root.
func safeKey(key string) bool {
	if key == "" {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if !safeRefPart(part) {
			return false
		}
	}
	return true
}

// LoadRevision is Load against a named mesh revision rather than the process
// default. Two revisions of the same map are separate cache entries whenever
// their files differ, so both can be resident at once — mind MaxCachedMeshes
// when they are.
func LoadRevision(mapName, revision string) (*Mesh, error) {
	src, err := revisionSource(revision)
	if err != nil {
		return nil, err
	}
	return hulls.load(src, mapName, hullAsset)
}

// Load returns the collision mesh for a map, or (nil, nil) when geometry is
// unavailable (disabled, unknown map, or no .tri published) — callers treat a
// nil mesh as "always visible". It never includes grenade clips, which block
// nothing but grenades. Results are cached process-wide, bounded to
// maxCachedMeshes built meshes.
func Load(mapName string) (*Mesh, error) {
	return hulls.load(defaultSource(), mapName, hullAsset)
}

// LoadGrenadeWorld loads what a thrown grenade collides with at a revision (""
// is the process default). Nil when the map has no hull; a build that
// publishes no grenade clips yields a world of the hull alone.
func LoadGrenadeWorld(mapName, revision string) (*GrenadeWorld, error) {
	src, err := revisionSource(revision)
	if err != nil {
		return nil, err
	}
	hull, err := hulls.load(src, mapName, hullAsset)
	if err != nil || hull == nil {
		return nil, err
	}
	clip, err := grenadeClips.load(src, mapName, grenadeClipAsset)
	if err != nil {
		return nil, err
	}
	world := NewGrenadeWorld(hull, clip)
	world.revision = src.label
	return world, nil
}

// load is keyed on the resolved URL rather than on revision and map, so the
// same map at two revisions never collides — the whole point of drift
// detection is that those two meshes differ — while a map the publisher
// deduped into an older build's file is built once for every build that
// shares it.
func (c *meshCache) load(src source, mapName string, a asset) (*Mesh, error) {
	name := normalizeMapName(mapName)
	if name == "" {
		return nil, nil
	}
	t := src.target(name, a)
	if t.url == "" {
		return nil, nil
	}
	c.mu.Lock()
	entry := c.entries[t.url]
	if entry == nil {
		entry = &cached{}
		c.entries[t.url] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() { entry.mesh, entry.err = fetchAndBuild(t) })

	mesh, err := entry.mesh, entry.err
	// A flat-layout probe can memoize "absent" for a URL a manifest also lists;
	// that answer is not the listed file's to give, so it is dropped and the
	// next load fetches it as listed.
	if t.listed && mesh == nil && err == nil {
		err = fmt.Errorf("mesh %s: listed in the manifest but not published", t.url)
	}

	c.mu.Lock()
	switch {
	case err != nil:
		// Drop failed attempts so the next parse retries. A memoized error is a
		// sync.Once that can never run again: one CDN timeout would otherwise
		// disable sightline validation for that map until the pod restarts.
		// An absent probe is not an error — that returns (nil, nil) and stays
		// memoized.
		if c.entries[t.url] == entry {
			delete(c.entries, t.url)
		}
	case mesh != nil:
		// Registered after the build so a mesh can never evict itself while it
		// is the one being loaded, and so absent meshes never displace a real
		// one. An entry evicted between the lookup above and here is simply
		// built again next time — the caller still gets a valid mesh.
		if c.entries[t.url] == entry {
			c.touch(t.url)
		}
	}
	c.mu.Unlock()
	return mesh, err
}

func fetchAndBuild(t target) (*Mesh, error) {
	resp, err := client.Get(t.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// B2 has no ListBucket grant on these keys, so a genuinely missing object
	// arrives as 403 as often as 404. For a probe both mean "no mesh"; for a
	// file the manifest lists they mean the publish is broken or not yet
	// visible, which must be retried rather than remembered.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		if t.listed {
			return nil, fmt.Errorf("mesh %s: listed in the manifest but answered %d", t.url, resp.StatusCode)
		}
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh %s: status %d", t.url, resp.StatusCode)
	}
	// The gzip is the artifact, not a transfer encoding, so it is undone here
	// rather than by net/http. Publishing it any other way does not survive the
	// CDN: a Cloudflare Worker's fetch strips Content-Encoding, and the edge
	// will not re-compress application/octet-stream.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mesh %s: %w", t.url, err)
	}
	defer gz.Close()
	data, err := io.ReadAll(io.LimitReader(gz, maxMeshBytes))
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
