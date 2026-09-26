import { describe, expect, it } from "vitest";

import {
  type EnrollmentPins,
  buildEnrollmentLabel,
  hasEnrollmentPins,
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
