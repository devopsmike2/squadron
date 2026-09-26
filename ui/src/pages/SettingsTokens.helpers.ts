// Pure helpers for the OpAMP enrollment-token pin builder, split out of
// SettingsTokens so the page file only exports its component (react-refresh) and
// so the label composition is unit-testable.

// EnrollmentPins are the operator-entered pin fields. Any subset may be set; a
// blank field is omitted. fleetId → pin: (ADR 0052 identity pin); env / cluster →
// env: / cluster: (ADR 0056 authenticated env/cluster binding).
export interface EnrollmentPins {
  fleetId: string;
  env: string;
  cluster: string;
}

// buildEnrollmentLabel composes the token LABEL that carries the pins, as the
// leading whitespace-separated run the server parses (ADR 0056): pin, then env,
// then cluster. Returns "" when no pin field is set. Values are trimmed; blank
// fields are skipped.
export const buildEnrollmentLabel = (p: EnrollmentPins): string => {
  const segs: string[] = [];
  if (p.fleetId.trim()) segs.push(`pin:${p.fleetId.trim()}`);
  if (p.env.trim()) segs.push(`env:${p.env.trim()}`);
  if (p.cluster.trim()) segs.push(`cluster:${p.cluster.trim()}`);
  return segs.join(" ");
};

// hasEnrollmentPins reports whether any pin field is set.
export const hasEnrollmentPins = (p: EnrollmentPins): boolean =>
  buildEnrollmentLabel(p) !== "";
