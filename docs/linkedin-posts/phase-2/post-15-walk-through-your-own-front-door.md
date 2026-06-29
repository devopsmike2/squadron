# Post 15: We deployed our own agent to find out if the front door works

**Pillar:** Dynamic discovery
**Tag at publish:** v0.89.304
**Visual evidence:** A screenshot of the Fleet / Agents view on the
live deployment at the v0.89.304 tag, showing the freshly
pipeline-deployed collector online — an `ip-172-31-…ec2.internal`
row, Online status, recent last-seen, registered with no manual
step. Optional inset: the `git log --oneline` for v0.89.301–304.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 300-450

## Draft

All week I've shown the proposer reasoning about collectors. The
volume spike it caught at minute zero. The decline note it carried
across clouds on Saturday. Every one of those posts quietly assumes
something I had never actually shown: that the collector reaches
Squadron in the first place.

This week I stopped assuming and deployed one myself.

The setup is the one a real team would use. A Terraform pipeline
stands up a host. Squadron opens a PR that injects its exporter into
the collector's config. The host boots, the agent starts shipping
telemetry, and it should appear in the fleet view. No manual step.

I ran the whole thing against a real EC2 box and watched the fleet.
Nothing showed up.

Walking the path to find out why produced four releases in an
afternoon. The load-bearing one:

The collector's exporter gzips its payload by default. Squadron's
ingest endpoint read the body and parsed it as protobuf without
checking for compression. So every standard collector got an HTTP
400 and its telemetry was dropped. Silently. The collector logged a
clean export. Squadron logged a wire-format parse error nobody was
watching. The agent just never appeared.

Two smaller ones rode along. The injector scaffolded a pipeline with
no receiver, so the collector refused to start. And discovery only
registered agents that emit a UUID service.instance.id — which most
infrastructure collectors don't — so it accepted their telemetry and
then quietly declined to file them in the fleet.

Fixed all three. Re-deployed. Watched the log say
`discovered telemetry-only agent` and the row come online.

Three more thoughts.

One. The unit tests were green the entire time. None of these bugs
existed in a test. They existed on the path a real operator walks —
deploy, inject, boot, ship. That gap is exactly the thing I keep
saying a green check does not cover.

Two. The failure mode that scares me is the silent one. A 202 on the
wire, telemetry on the floor, nothing in the log an operator would
read. The honest-framing posts from this week were about the
proposer admitting what it cannot see. Same discipline has to apply
to the platform's own front door, or the rest is theater.

Three. The fix is not just our bug. It widens who the product is
for. Any standard OTLP collector now registers without ceremony — no
special config, no UUID it was never going to set. Deploy it, it
shows up.

Squadron is open source. Repo in the comments.

What's a bug you only found because you ran the whole path yourself,
end to end, instead of trusting the green check?

#OpenTelemetry #SRE
