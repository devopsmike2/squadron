import { describe, expect, it } from "vitest";

import {
  type PermRow,
  permRowsToPermissions,
} from "./SettingsIdentity.helpers";

const row = (over: Partial<PermRow> = {}): PermRow => ({
  scope: "rollouts:write",
  resource_type: "",
  allResources: true,
  resourceIdsText: "",
  env: "",
  cluster: "",
  ...over,
});

describe("permRowsToPermissions — ADR 0053 label_match", () => {
  it("omits label_match when env and cluster are blank", () => {
    const [p] = permRowsToPermissions([row()]);
    expect(p.label_match).toBeUndefined();
    expect(p).toMatchObject({
      scope: "rollouts:write",
      all_resources: true,
      resource_ids: [],
    });
  });

  it("sets deployment.environment when env is given", () => {
    const [p] = permRowsToPermissions([row({ env: " prod " })]);
    expect(p.label_match).toEqual({ "deployment.environment": "prod" });
  });

  it("sets k8s.cluster.name when cluster is given", () => {
    const [p] = permRowsToPermissions([row({ cluster: "us-west-2" })]);
    expect(p.label_match).toEqual({ "k8s.cluster.name": "us-west-2" });
  });

  it("sets both when env and cluster are given", () => {
    const [p] = permRowsToPermissions([
      row({ env: "prod", cluster: "us-west-2" }),
    ]);
    expect(p.label_match).toEqual({
      "deployment.environment": "prod",
      "k8s.cluster.name": "us-west-2",
    });
  });

  it("drops rows with a blank scope", () => {
    expect(permRowsToPermissions([row({ scope: "  " })])).toHaveLength(0);
  });

  it("parses explicit resource_ids only when all_resources is false", () => {
    const [p] = permRowsToPermissions([
      row({ allResources: false, resourceIdsText: "a, b\nc" }),
    ]);
    expect(p.all_resources).toBe(false);
    expect(p.resource_ids).toEqual(["a", "b", "c"]);
  });
});
