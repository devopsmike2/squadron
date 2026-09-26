import { describe, expect, it } from "vitest";

import {
  type EnrollmentPins,
  buildEnrollmentLabel,
  hasEnrollmentPins,
  parseEnrollmentLabel,
} from "./SettingsTokens.helpers";

const pins = (over: Partial<EnrollmentPins> = {}): EnrollmentPins => ({
  fleetId: "",
  env: "",
  cluster: "",
  ...over,
});

describe("buildEnrollmentLabel — ADR 0052/0056 pin label", () => {
  it("returns empty when no field is set", () => {
    expect(buildEnrollmentLabel(pins())).toBe("");
    expect(hasEnrollmentPins(pins())).toBe(false);
  });

  it("composes pin, env, cluster in leading-run order", () => {
    expect(
      buildEnrollmentLabel(
        pins({ fleetId: "fleet-abc", env: "prod", cluster: "us-west-2" }),
      ),
    ).toBe("pin:fleet-abc env:prod cluster:us-west-2");
  });

  it("emits only the fields that are set", () => {
    expect(buildEnrollmentLabel(pins({ env: "prod" }))).toBe("env:prod");
    expect(buildEnrollmentLabel(pins({ cluster: "eu-1" }))).toBe(
      "cluster:eu-1",
    );
    expect(buildEnrollmentLabel(pins({ fleetId: "f1" }))).toBe("pin:f1");
  });

  it("trims each value", () => {
    expect(buildEnrollmentLabel(pins({ env: "  prod  " }))).toBe("env:prod");
  });

  it("hasEnrollmentPins is true when any field is set", () => {
    expect(hasEnrollmentPins(pins({ cluster: "eu-1" }))).toBe(true);
  });
});

describe("parseEnrollmentLabel — inverse of buildEnrollmentLabel", () => {
  it("reads no pins from a plain label", () => {
    const r = parseEnrollmentLabel("ci-bot");
    expect(r.hasPins).toBe(false);
    expect(r.pins).toEqual(pins());
    expect(r.rest).toBe("ci-bot");
  });

  it("round-trips a fully pinned label", () => {
    const p = pins({ fleetId: "fleet-abc", env: "prod", cluster: "us-west-2" });
    const r = parseEnrollmentLabel(buildEnrollmentLabel(p));
    expect(r.hasPins).toBe(true);
    expect(r.pins).toEqual(p);
    expect(r.rest).toBe("");
  });

  it("reads a partial pin run", () => {
    const r = parseEnrollmentLabel("env:prod");
    expect(r.pins.env).toBe("prod");
    expect(r.pins.fleetId).toBe("");
    expect(r.pins.cluster).toBe("");
    expect(r.rest).toBe("");
  });

  it("keeps trailing free text as rest and stops at the first non-pin segment", () => {
    const r = parseEnrollmentLabel("pin:f1 env:prod ci notes here");
    expect(r.pins.fleetId).toBe("f1");
    expect(r.pins.env).toBe("prod");
    expect(r.rest).toBe("ci notes here");
  });

  it("treats a mid-label pin prefix as free text, not a pin", () => {
    const r = parseEnrollmentLabel("ci-bot env:prod");
    expect(r.hasPins).toBe(false);
    expect(r.rest).toBe("ci-bot env:prod");
  });
});
