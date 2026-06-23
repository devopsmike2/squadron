// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// relativeTime — shared helper for the v0.89.77 trace integration
// slice 1 chunk 4 per-row "Last seen" column. Renders an ISO-8601
// timestamp (the discovery scan response's `last_seen_at` field) as
// a compact relative-time string with a sentinel "never" branch for
// undefined / null inputs.
//
// Threshold design:
//   - < 60s          → "Xs ago"
//   - < 60m          → "Xm ago"
//   - < 24h          → "Xh ago"
//   - < 7d           → "Xd ago"
//   - < 30d          → "Xw ago"  (round number weeks — 7-day buckets)
//   - >= 30d         → ISO date (YYYY-MM-DD)
//
// The 1w/30d switch picks round-number weeks rather than calendrical
// months for two reasons: (a) "Xw ago" stays compact in the table
// column (1-2 chars + "w ago" vs spelled-out months); (b) the
// dashboard's primary signal is "is this recent" — past 30 days
// the operator is reading a date for forensics, not a duration. The
// helper degrades to a stable YYYY-MM-DD format the row's tooltip
// can extend with a full timestamp.
//
// All thresholds use round-number division (floor) — no calendrical
// edge cases (DST, leap seconds) factor in. A timestamp exactly 60
// seconds ago renders "1m ago", not "60s ago"; 24 hours exactly
// renders "1d ago", not "24h ago". Boundary tests pin this.
//
// The helper is pure and clock-injectable: callers in production
// pass Date.now() implicitly (the default `now` parameter);
// tests pass a fixed nowMs so the threshold transitions are
// deterministic.

export interface RelativeTime {
  // text is the operator-visible label ("5m ago" / "never" / "2026-06-23").
  text: string;
  // isNever signals the undefined / null input branch — the
  // Inventory cell uses this to attach the warning indicator.
  isNever: boolean;
}

const NEVER: RelativeTime = { text: "never", isNever: true };

export function relativeTime(
  iso: string | undefined | null,
  nowMs?: number,
): RelativeTime {
  if (iso === undefined || iso === null || iso === "") {
    return NEVER;
  }
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) {
    return NEVER;
  }
  const now = nowMs ?? Date.now();
  const deltaSec = Math.max(0, Math.floor((now - ts) / 1000));

  if (deltaSec < 60) {
    return { text: `${deltaSec}s ago`, isNever: false };
  }
  const minutes = Math.floor(deltaSec / 60);
  if (minutes < 60) {
    return { text: `${minutes}m ago`, isNever: false };
  }
  const hours = Math.floor(deltaSec / 3600);
  if (hours < 24) {
    return { text: `${hours}h ago`, isNever: false };
  }
  const days = Math.floor(deltaSec / 86400);
  if (days < 7) {
    return { text: `${days}d ago`, isNever: false };
  }
  if (days < 30) {
    const weeks = Math.floor(days / 7);
    return { text: `${weeks}w ago`, isNever: false };
  }
  // Past 30 days: render the ISO date verbatim — the operator is
  // doing forensics, not estimating recency. YYYY-MM-DD strips the
  // time component so the column stays compact; the row tooltip can
  // surface the full timestamp.
  const d = new Date(ts);
  const yyyy = d.getUTCFullYear();
  const mm = String(d.getUTCMonth() + 1).padStart(2, "0");
  const dd = String(d.getUTCDate()).padStart(2, "0");
  return { text: `${yyyy}-${mm}-${dd}`, isNever: false };
}
