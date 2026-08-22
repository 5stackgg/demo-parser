## 5Stack Demo Parser

5Stack is a platform for organizing and managing competitive CS2 matches and tournaments.

The demo parser is a small HTTP + CLI service that wraps [markus-wa/demoinfocs-golang](https://github.com/markus-wa/demoinfocs-golang) to extract playback metadata, events, and player stats from a CS2 `.dem` file.

Please visit [5Stack](https://docs.5stack.gg) for more documentation.

### Endpoints

| Endpoint | Body | Answers |
| --- | --- | --- |
| `POST /parse` | `{"demo_url": "..."}` | parsed `Result` JSON |
| `POST /parse-file` | multipart, `demo` part | parsed `Result` JSON |
| `POST /smoke-volume` | `{"map","x","y","z"}` | the bloom at a point |
| `POST /sightlines` | map + smoke(s) + eye-position pairs | what the smoke blocks |
| `POST /oneway` | map + smoke(s) + player-position pairs | asymmetric visibility |
| `POST /drift` | map + two mesh revisions + lineups | which lineups a map update moved |

The last four answer questions about a **map**, not a demo, so they work for a
lineup nobody has ever thrown. They need the map's collision mesh and return
`404` when none is published for it. Coordinates are raw CS2 source units.

`/smoke-volume` returns the same voxel grid the playback blob carries
(`ox/oy/oz`, `vs`, `dx/dy/dz`, `den`), so one decoder serves both, plus `cells`
and `radius`. A point that resolves inside geometry returns `422`.

`/sightlines` takes `at` (a detonation point), `smoke`, or `smokes` — a cloud is
either `{"at": {...}}` or `{"volume": {...}}`, where the volume is one
`/smoke-volume` handed straight back to skip the flood. Each pair returns
`blocked`, `blocked_by` (`world` / `smoke`), `depth` (optical depth in cell
widths of full density), `transmittance` (`e^-depth`) and `world_blocked`.
`threshold` defaults to `3.0` — about 5% of the target's contrast surviving —
and is a request field because it is a judgement, not a measurement.

`/oneway` takes player positions (feet by default, `"positions": "eyes"`
otherwise) and tests both standing and crouched eye heights in both directions,
treating each player as a body rather than a point. It reports `one_way`,
`favors`, `cause`, `confidence`, `contested`, the `best` stance pairing and all
four pairings, along with `caveats` describing what the model does not know.

### `POST /drift` — map-patch drift detection

When Valve ships a map update, every stored lineup is re-flown against the old
collision mesh and the new one, and the two endpoints are compared.

**The output is a differential and nothing else.** The grenade simulator behind
it is deterministic and self-consistent but *not* fitted to CS2: an absolute
landing point out of it is wrong by an unknown amount. It is sound here only
because the same model error appears on both sides and cancels. The position
field is called `comparison_point` for that reason, and **no coordinate from
this endpoint may ever be shown to a player as where their nade lands.**

```jsonc
POST /drift
{
  "map": "de_mirage",
  "from": "17595823-4",          // mesh revision before the patch
  "to":   "17595823-5",          // after it; "" means the revision this process is pinned to
  "lineups": [
    {
      "id": "1f4c…",
      "nade_type": "Smoke",                                  // Smoke | HE | Flash | Molotov | Decoy
      "initial_position": {"x": -2300, "y": 0, "z": -64},    // both optional; a lineup
      "initial_velocity": {"x": 500,  "y": -80, "z": 200}    // missing either is unsimulatable
    }
  ],
  "stream": false,               // true → NDJSON, required above 2000 lineups
  "unchanged_radius": 8,         // optional threshold overrides
  "major_radius": 64,
  "constants": {"gravity": 320}  // optional physics overrides, applied to BOTH sides
}
```

A **mesh revision** is a jsDelivr tag (`17595823-5`), an `owner/repo@tag`, or an
`http(s)` base for a mirror. `from` and `to` come back **resolved**, so a blank
one is legible in the report later.

Each lineup gets one of four verdicts:

| verdict | meaning |
| --- | --- |
| `unchanged` | both meshes end the flight in the same place, within `unchanged_radius` |
| `moved` | both resolve, and the endpoint shifted — `severity` is `minor` or `major` |
| `broken` | it resolved on the old mesh and does not on the new one: `inside_geometry`, `start_sealed`, `out_of_world`, or never comes to rest |
| `unsimulatable` | no recorded seed, an unknown grenade, or a flight that fails on **both** meshes — which says nothing about the map |

```jsonc
{
  "map": "de_mirage",
  "from": "…", "to": "…",
  "constants": { … },            // echoed, so a report can be reproduced
  "thresholds": {"unchanged": 8, "major": 64},
  "summary": {"lineups": 900, "unchanged": 848, "moved": 9, "broken": 2, "unsimulatable": 41, "max_distance": 213.4},
  "results": [
    {
      "index": 0,                // request order, for matching when ids repeat
      "id": "1f4c…",
      "verdict": "moved",
      "severity": "minor",
      "reason": "landing moved 21.7 units",
      "from": {"comparison_point": {"x":…,"y":…,"z":…}, "resolved": true, "stop": "rest",
               "bounces": 3, "flight_seconds": 3.61, "steps": 462},
      "to":   { … },
      "distance": 21.7, "distance_xy": 21.6, "distance_z": 1.9   // absent unless BOTH sides resolved
    }
  ],
  "caveats": ["…"]               // ships with the payload; surface it wherever the numbers are shown
}
```

`"stream": true` returns `application/x-ndjson`: one `{"type":"header"}` line,
one `{"type":"result"}` line per lineup **in request order**, then
`{"type":"summary"}`. The status code is sent before the first result, so a run
that fails part way through ends with a `{"type":"error"}` line — a consumer
that does not check for one will read a truncated run as a clean one.

**Thresholds are judgements, not measurements.** `unchanged` is 8 units: two
re-exports of an unchanged map still round vertices differently, and a bounce
grazing that seam lands a few units off. `major` is 64 units — a smoke's radius
is 144 and a player is 32 wide, so past 64 a cloud no longer covers the same
gap. Both are request fields.

**Batch sizing.** A lineup costs two flights: ~2 ms on one core, ~0.4 ms across
eight (measured on de_mirage, 138k triangles). 500-1000 per request is the
comfortable size; 2000 is the cap for a single JSON body and 10000 the hard cap
with `stream`. Drift is the only endpoint that holds **two** meshes at once —
21 MiB for the de_mirage pair, 136 MiB for de_anubis — so it is serialized to
one request at a time. Raise `DRIFT_CONCURRENCY` only on a pod dedicated to it,
and `DRIFT_WORKERS` (default `min(NumCPU, 8)`) to change per-request
parallelism; neither changes the answer.

Mesh loading is process-wide, concurrency-safe and LRU-bounded; raise
`MAP_MESH_CACHE` (default 2) on a deployment serving lineups across many maps.
Drift needs at least 2 or it refetches a mesh per request, and 4 is a better
number on a pod that also serves the other endpoints.
