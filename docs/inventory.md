# Inventory Reconciliation (v0.32)

Squadron's inventory surface answers a specific question SRE teams
care about: **"the CI pipeline said it deployed to 80 hosts — are
all 80 actually checking in?"**

## The model

Squadron stores two views of the fleet:

- **Expected** — a list of hostnames declared by some CI/CD
  pipeline. Pushed via the inventory API at the end of a deploy
  job.
- **Actual** — agents that dialed in via OpAMP and ended up in the
  agents table.

The reconciliation service diffs them on every read and classifies
each hostname into one of three buckets:

- **`healthy`** — expected and recently seen.
- **`missing`** — expected but never connected, or quiet for more
  than 10 minutes.
- **`unexpected`** — connected but not in the expected list.
  Usually means a manual install or a stray host the CI pipeline
  doesn't know about.

Hostname matching is case-insensitive and FQDN-tolerant — if the
expected entry says `host01` and the agent reports
`host01.example.com`, they match.

## CI integration

The bulk-rotate endpoint is the recommended path for any
deploy-time pipeline. Send the entire target list with a stable
`source` identifier; Squadron drops every existing row tagged with
that source and inserts the new list atomically.

```bash
# In your GitHub Actions / Jenkins / GitLab pipeline, after the
# deploy step:

curl -X PUT https://squadron.example.com/api/v1/inventory/expected \
  -H "Authorization: Bearer $SQUADRON_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "source": "gha-otel-deploy",
    "entries": [
      { "hostname": "host01", "labels": {"env": "prod"} },
      { "hostname": "host02", "labels": {"env": "prod"} },
      { "hostname": "host03", "labels": {"env": "staging"},
        "notes": "from job #1234" }
    ]
  }'
```

Multiple pipelines coexist because each owns its slice of the
inventory under a different `source`. Squadron's unified view
(no source filter) shows all of them; the source-scoped view
(`?source=gha-otel-deploy`) hides hosts owned by other pipelines.

## Endpoints

All endpoints require auth scopes:

- `GET /api/v1/inventory/reconciliation[?source=X]` —
  `ScopeAgentsRead`. The full diff report with row-level detail.
- `GET /api/v1/inventory/expected[?source=X]` — `ScopeAgentsRead`.
  Just the expected list.
- `POST /api/v1/inventory/expected` — `ScopeAgentsWrite`. Upsert
  one row (for ad-hoc additions from squadronctl or the UI).
- `PUT /api/v1/inventory/expected` — `ScopeAgentsWrite`. Bulk
  rotate. The CI path.
- `DELETE /api/v1/inventory/expected/:hostname` —
  `ScopeAgentsWrite`. Remove one row.

## UI surfaces

- **Dashboard** has an inventory stacked bar showing the count in
  each bucket. Null when no expected rows have been submitted.
- **`/inventory`** has a per-host table with status badges, source
  attribution, last-seen times, and notes from the CI pipeline.

## Storage

The `expected_agents` table is a simple key-value rotation table.
Each row is one (hostname, labels, source, notes) tuple — labels
serialized as JSON because hostname is the natural key.

No retention story is needed; CI pipelines re-push their entire
list on each deploy, so dead entries get rotated out naturally.
Manual entries (POST) stay until you explicitly DELETE them.

## Roadmap

v0.33 layers webhook alerts on top of this surface so a
healthy → missing transition pages the on-call automatically. v0.34
adds a CSV import / export to bridge the gap for pipelines that
can't reach the Squadron API directly.

See also `docs/pipeline-health.md` for the sibling "are agents
actually delivering data" surface.
