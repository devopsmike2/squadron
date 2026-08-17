// squadronctl command builders.
//
// The UI funnels operators to the squadronctl CLI: alongside the key
// agent/group actions we surface the exact command that reproduces the
// same operation. These helpers turn action params (agent id, group
// name/id, config id) into the canonical command string so the copied
// text always matches the CLI's real verbs (see cmd/squadronctl and
// docs/squadronctl.md).
//
// The emitted command is the operation itself. SQUADRON_URL /
// SQUADRON_TOKEN are assumed to be set in the environment; the CLI
// onboarding page (/settings/cli) explains that setup once, so we do
// not repeat the auth preamble on every snippet.

const CLI = "squadronctl";

// shellQuote wraps a value in single quotes when it contains anything
// outside the safe unquoted set (group names can carry spaces or other
// shell-significant characters; agent/config ids are opaque tokens).
// Embedded single quotes are escaped the POSIX way ('\'').
export function shellQuote(value: string): string {
  if (value === "") return "''";
  if (/^[A-Za-z0-9_./:@=+-]+$/.test(value)) return value;
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

// Assign an agent to a group. The CLI accepts a group id OR a group
// name for --group, so we prefer the human-readable name when we have
// it and fall back to the id.
export function setGroupCommand(
  agentId: string,
  groupNameOrId: string,
): string {
  return `${CLI} agents set-group ${shellQuote(agentId)} --group ${shellQuote(
    groupNameOrId,
  )}`;
}

// Clear an agent's group assignment (the explicit form of
// `set-group --none`).
export function clearGroupCommand(agentId: string): string {
  return `${CLI} agents clear-group ${shellQuote(agentId)}`;
}

// Clear an agent's agent-scoped config so resolution falls back to the
// group config.
export function clearConfigCommand(agentId: string): string {
  return `${CLI} agents clear-config ${shellQuote(agentId)}`;
}

// Restart one agent over OpAMP.
export function restartAgentCommand(agentId: string): string {
  return `${CLI} agents restart ${shellQuote(agentId)}`;
}

// Assign a config to a group.
export function assignConfigCommand(groupId: string, configId: string): string {
  return `${CLI} groups assign-config ${shellQuote(groupId)} --config ${shellQuote(
    configId,
  )}`;
}

// Print an agent's reported effective (merged) config.
export function getEffectiveCommand(agentId: string): string {
  return `${CLI} agents get ${shellQuote(agentId)} --effective`;
}

// List agents (optionally scoped to a group by name or id).
export function listAgentsCommand(groupNameOrId?: string): string {
  return groupNameOrId
    ? `${CLI} agents list --group ${shellQuote(groupNameOrId)}`
    : `${CLI} agents list`;
}
