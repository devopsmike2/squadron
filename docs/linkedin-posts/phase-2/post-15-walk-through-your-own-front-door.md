# Post 15: We deployed our own agent to find out if the front door works

**Pillar:** Dynamic discovery
**Tag at publish:** v0.89.304
**Visual evidence:** A screenshot of the Fleet / Agents page on the
live deployment at the v0.89.304 tag, showing the freshly
pipeline-deployed collector online — an `ip-172-31-…ec2.internal`
row with an Online status and a recent last-seen, registered with
no manual step. A small inset frame shows the `git log --oneline`
for the four tags this produced: v0.89.301, v0.89.302, v0.89.303,
v0.89.304. The terminal text + the Online row are the evidence;
no mockup.
**Hashtags:** #OpenTelemetry #SRE
**Target word count:** 200-400

## Draft

The promise on the box is "deploy a collector and it shows up in
your fleet." The only way to know if that is true is to deploy one
yourself and watch.

So we did. A Terraform pipeline stands up a host, Squadron opens a
PR that injects the `otlphttp` exporter into the collector config,
the host boots, the agent ships telemetry, and it should appear in
Fleet. We walked the whole path on a real EC2 instance. It did not
appear. Walking the path produced four releases.

**v0.89.301 — the injector wrote a pipeline with no receiver.** The
config-injection scaffolded an exporter into a signal pipeline that
had no receiver. otelcol fails that config at startup, so the agent
never ran. Caught the moment a real collector tried to boot it.

**v0.89.302 — the receiver did not decompress gzip.** The OTel
`otlphttp` exporter gzips request bodies by default. Squadron's
OTLP/HTTP receiver read the body and unmarshalled it as protobuf
without checking `Content-Encoding`, so every compressed request —
i.e. every standard client out of the box — got an HTTP 400 and
its telemetry was dropped. Silently. This one would have hit
everybody.

**v0.89.303 / .304 — discovery only registered agents with a UUID.**
Passive OTLP discovery keyed agent identity off a UUID
`service.instance.id`. Most infra and hostmetrics collectors do not
set one. Squadron accepted their telemetry (202) and then skipped
registering them, with no log. v0.89.303 added the log; v0.89.304
synthesizes a stable identity from `host.name`, so a plain
collector now registers. We re-deployed and watched the agent come
online — `discovered telemetry-only agent` in the log, the row in
Fleet.

None of these was catastrophic alone. Together they were the
difference between "deploy a collector and it appears" and "deploy
a collector and nothing happens, with no error to explain why."
Unit tests passed the whole time. The bugs only existed on the
path a real operator takes.

The front door only counts if you walk through it.

Repo at the v0.89.304 tag. The fleet surface is at `/fleet`.

#OpenTelemetry #SRE
