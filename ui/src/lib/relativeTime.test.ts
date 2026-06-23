// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import { relativeTime } from "./relativeTime";

// relativeTime.test.ts — v0.89.77 trace integration slice 1 chunk 4.
// Pins the threshold transitions so a future tweak to the bucket
// boundaries fails this suite first.

describe("relativeTime", () => {
  // nowFixed is a deterministic clock the table-driven cases use to
  // exercise each bucket without depending on wallclock drift. The
  // value is 2026-06-23T14:32:00Z, the canonical "now" from the
  // design doc §7 example payload.
  const nowFixed = Date.parse("2026-06-23T14:32:00Z");

  it("TestRelativeTime_UndefinedYieldsNever", () => {
    expect(relativeTime(undefined, nowFixed)).toEqual({
      text: "never",
      isNever: true,
    });
  });

  it("TestRelativeTime_NullYieldsNever", () => {
    expect(relativeTime(null, nowFixed)).toEqual({
      text: "never",
      isNever: true,
    });
  });

  it("TestRelativeTime_EmptyStringYieldsNever", () => {
    expect(relativeTime("", nowFixed)).toEqual({
      text: "never",
      isNever: true,
    });
  });

  it("TestRelativeTime_InvalidISOYieldsNever", () => {
    expect(relativeTime("not-a-date", nowFixed)).toEqual({
      text: "never",
      isNever: true,
    });
  });

  it("TestRelativeTime_VariousIntervals", () => {
    const cases: Array<{
      name: string;
      isoOffsetMs: number; // negative offset from nowFixed
      want: string;
    }> = [
      { name: "5 seconds ago", isoOffsetMs: 5 * 1000, want: "5s ago" },
      { name: "59 seconds ago", isoOffsetMs: 59 * 1000, want: "59s ago" },
      { name: "60 seconds rolls to 1m ago", isoOffsetMs: 60 * 1000, want: "1m ago" },
      { name: "30 minutes ago", isoOffsetMs: 30 * 60 * 1000, want: "30m ago" },
      { name: "59 minutes ago", isoOffsetMs: 59 * 60 * 1000, want: "59m ago" },
      { name: "60 minutes rolls to 1h ago", isoOffsetMs: 60 * 60 * 1000, want: "1h ago" },
      { name: "3 hours ago", isoOffsetMs: 3 * 60 * 60 * 1000, want: "3h ago" },
      { name: "23 hours ago", isoOffsetMs: 23 * 60 * 60 * 1000, want: "23h ago" },
      { name: "24 hours rolls to 1d ago", isoOffsetMs: 24 * 60 * 60 * 1000, want: "1d ago" },
      { name: "2 days ago", isoOffsetMs: 2 * 24 * 60 * 60 * 1000, want: "2d ago" },
      { name: "6 days ago", isoOffsetMs: 6 * 24 * 60 * 60 * 1000, want: "6d ago" },
      { name: "7 days rolls to 1w ago", isoOffsetMs: 7 * 24 * 60 * 60 * 1000, want: "1w ago" },
      { name: "3 weeks ago", isoOffsetMs: 21 * 24 * 60 * 60 * 1000, want: "3w ago" },
      { name: "29 days ago is still weeks", isoOffsetMs: 29 * 24 * 60 * 60 * 1000, want: "4w ago" },
    ];
    for (const c of cases) {
      const iso = new Date(nowFixed - c.isoOffsetMs).toISOString();
      const got = relativeTime(iso, nowFixed);
      expect(got).toEqual({ text: c.want, isNever: false });
    }
  });

  it("TestRelativeTime_BeyondThirtyDaysRendersDate", () => {
    // 45 days back from nowFixed lands at 2026-05-09.
    const isoOffsetMs = 45 * 24 * 60 * 60 * 1000;
    const iso = new Date(nowFixed - isoOffsetMs).toISOString();
    const got = relativeTime(iso, nowFixed);
    expect(got.isNever).toBe(false);
    // YYYY-MM-DD shape — exact date depends on tz but the format is
    // pinned.
    expect(got.text).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });

  it("TestRelativeTime_FutureTimestampClampsToZero", () => {
    // If somehow the input is ahead of now (clock skew), the helper
    // floors deltaSec to 0 and renders "0s ago" rather than a
    // negative value.
    const future = new Date(nowFixed + 5_000).toISOString();
    const got = relativeTime(future, nowFixed);
    expect(got).toEqual({ text: "0s ago", isNever: false });
  });
});
