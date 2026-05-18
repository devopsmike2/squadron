# Squadron marketing assets

Source files for the README gallery + future demo video.

## Re-capture the screenshots

The Playwright script in `capture.mjs` drives a headless Chromium
through Squadron's marketing-relevant routes (`/quickstart`,
`/savings`, `/cost-insights`, etc.) and saves polished 1440×900
PNG screenshots to `scenes/`. Re-run any time the UI changes:

```bash
cd marketing
npm install              # one-time
node capture.mjs         # writes scenes/*.png
```

Requires a running Squadron at `http://localhost:5173` (the dev
UI; if you've changed ports, edit `BASE` at the top of
`capture.mjs`). Re-runs are idempotent — they overwrite the
existing screenshots.

## Files

- `capture.mjs` — Playwright capture script. Lists every scene
  the README gallery uses, with selectors + after-load hooks for
  scenes that need a click-to-expand step.
- `package.json` — pins `playwright` for the capture script.
  Dev-only; not part of the Squadron deliverable.
- `scenes/*.png` — the actual marketing screenshots that the
  top-level README embeds. Commit changes to these alongside the
  UI changes they reflect.

## Future video work

When you're ready to produce a narrated mp4 demo, see
`docs/demo-script.md` for the 90-second script. The capture
script in this directory is the right starting point — extend it
to record short MP4 clips per scene using Playwright's tracing /
video recording features, then composite with `ffmpeg` (already
documented in the script's production notes).
