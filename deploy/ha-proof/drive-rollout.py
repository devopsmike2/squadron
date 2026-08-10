#!/usr/bin/env python3
"""Drive a cross-instance rollout for the HA proof harness.

Creates a NEW target config for the harness group and starts a single-stage
(100%, convergence-gated) rollout via the API. Point --api at ANY instance
(the rollout engine runs on whichever instance is the elected leader).

Usage:
  drive-rollout.py [--api http://127.0.0.1:18081] [--group ha-proof-group]
"""
import argparse
import json
import sys
import urllib.request


def call(base, path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method=method,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=10) as r:
        return r.status, json.loads(r.read() or "null")


TARGET = """receivers:
  otlp:
    protocols:
      grpc: { endpoint: 0.0.0.0:4317 }
      http: { endpoint: 0.0.0.0:4318 }
processors:
  batch: { timeout: 7s, send_batch_size: 4096 }
exporters:
  debug: {}
service:
  pipelines:
    metrics: { receivers: [otlp], processors: [batch], exporters: [debug] }
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://127.0.0.1:18081")
    ap.add_argument("--group", default="ha-proof-group")
    ap.add_argument("--version", type=int, default=2)
    args = ap.parse_args()

    # Resolve the group id from any connected agent's group_name.
    _, agents = call(args.api, "/api/v1/agents")
    gid = None
    for a in agents.get("items", []):
        if a.get("group_name") == args.group:
            gid = a.get("group_id")
            break
    if not gid:
        print(f"no agent found in group {args.group}; connect agentsim first", file=sys.stderr)
        sys.exit(1)

    st, cfg = call(args.api, "/api/v1/configs", "POST",
                   {"name": "ha-proof-target", "group_id": gid,
                    "content": TARGET, "version": args.version})
    print("create config:", st, "id", cfg["id"], "hash", (cfg.get("config_hash") or "")[:16])

    ro = {"name": "ha-proof-xinstance", "group_id": gid,
          "target_config_id": cfg["id"],
          "stages": [{"mode": "percent", "percentage": 100,
                      "dwell_seconds": 5, "convergence_percent": 100}],
          # lenient abort so transient pre-convergence drift does not abort
          "abort_criteria": {"max_drifted_agents": 1000,
                             "min_dwell_seconds_before_abort": 3600}}
    st, r = call(args.api, "/api/v1/rollouts", "POST", ro)
    print("create rollout:", st, "id", r["id"], "state", r["state"])
    print("ROLLOUT_ID=" + r["id"])


if __name__ == "__main__":
    main()
