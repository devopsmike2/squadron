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

// ParsedEnrollmentLabel is the inverse of buildEnrollmentLabel: the pins read
// off an existing token's label, plus whatever free text trails the leading pin
// run.
export interface ParsedEnrollmentLabel {
  pins: EnrollmentPins;
  hasPins: boolean;
  // rest is the label text after the leading pin run (an ordinary label has all
  // of its text here and no pins).
  rest: string;
}

// parseEnrollmentLabel reads the leading whitespace-separated run of
// pin:/env:/cluster: segments off a token label, mirroring the server's parse
// (ADR 0056). Parsing stops at the first segment without a known prefix; the
// remainder is returned as rest. A plain label yields no pins and rest === label.
export const parseEnrollmentLabel = (label: string): ParsedEnrollmentLabel => {
  const pins: EnrollmentPins = { fleetId: "", env: "", cluster: "" };
  const parts = label.trim().split(/\s+/);
  let i = 0;
  for (; i < parts.length; i++) {
    const seg = parts[i];
    if (seg.startsWith("pin:")) pins.fleetId = seg.slice("pin:".length);
    else if (seg.startsWith("env:")) pins.env = seg.slice("env:".length);
    else if (seg.startsWith("cluster:"))
      pins.cluster = seg.slice("cluster:".length);
    else break;
  }
  return {
    pins,
    hasPins: hasEnrollmentPins(pins),
    rest: parts.slice(i).join(" "),
  };
};
