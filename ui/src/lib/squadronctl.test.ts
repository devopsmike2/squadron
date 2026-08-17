import { describe, it, expect } from "vitest";

import {
  assignConfigCommand,
  clearConfigCommand,
  clearGroupCommand,
  getEffectiveCommand,
  listAgentsCommand,
  restartAgentCommand,
  setGroupCommand,
  shellQuote,
} from "./squadronctl";

describe("squadronctl command builders", () => {
  it("builds set-group with an agent id and group name", () => {
    expect(setGroupCommand("agent-123", "prod-collectors")).toBe(
      "squadronctl agents set-group agent-123 --group prod-collectors",
    );
  });

  it("quotes a group name that contains spaces", () => {
    expect(setGroupCommand("agent-123", "US East Prod")).toBe(
      "squadronctl agents set-group agent-123 --group 'US East Prod'",
    );
  });

  it("builds clear-group", () => {
    expect(clearGroupCommand("agent-123")).toBe(
      "squadronctl agents clear-group agent-123",
    );
  });

  it("builds clear-config", () => {
    expect(clearConfigCommand("agent-123")).toBe(
      "squadronctl agents clear-config agent-123",
    );
  });

  it("builds restart", () => {
    expect(restartAgentCommand("agent-123")).toBe(
      "squadronctl agents restart agent-123",
    );
  });

  it("builds assign-config", () => {
    expect(assignConfigCommand("grp-1", "cfg-9")).toBe(
      "squadronctl groups assign-config grp-1 --config cfg-9",
    );
  });

  it("builds get --effective", () => {
    expect(getEffectiveCommand("agent-123")).toBe(
      "squadronctl agents get agent-123 --effective",
    );
  });

  it("builds agents list, scoped and unscoped", () => {
    expect(listAgentsCommand()).toBe("squadronctl agents list");
    expect(listAgentsCommand("prod")).toBe(
      "squadronctl agents list --group prod",
    );
  });

  it("leaves safe tokens unquoted and escapes embedded single quotes", () => {
    expect(shellQuote("prod-collectors")).toBe("prod-collectors");
    expect(shellQuote("a b")).toBe("'a b'");
    expect(shellQuote("it's")).toBe("'it'\\''s'");
  });
});
