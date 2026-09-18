// Pure helpers for label-mode rollout stage selectors.
//
// A label-mode stage targets agents by an AND'd set of key=value label
// rows (see StageEditor in pages/Rollouts.tsx). The facet quick-pick
// (Environment / Cluster) writes into those same rows, so the upsert logic
// lives here as pure functions — testable without rendering the builder and
// reusable by any future facet dimension.

export type SelectorRow = { key: string; value: string };

// Current value of a given label key in a stage's selector rows, or "" if
// the key isn't set.
export function selectorValueFor(rows: SelectorRow[], key: string): string {
  return rows.find((r) => r.key === key)?.value ?? "";
}

// Upsert (value non-empty) or remove (value empty) the row for `key`,
// dropping any blank placeholder row along the way, and always leaving at
// least one row so the editor keeps an input pair to type into.
export function applyScope(
  rows: SelectorRow[],
  key: string,
  value: string,
): SelectorRow[] {
  let next = rows.filter(
    (r) => r.key !== key && !(r.key === "" && r.value === ""),
  );
  if (value) next = [...next, { key, value }];
  if (next.length === 0) next = [{ key: "", value: "" }];
  return next;
}
