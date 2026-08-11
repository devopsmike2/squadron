#!/usr/bin/env python3
"""otel-cred-exec.py — generic credential-injecting exec shim (Python variant).

Same contract as otel-cred-exec.sh: the OpAMP supervisor launches this file as
`agent.executable`, passing `--config <supervisor-written-config>` (and any
feature-gate flags) as arguments. This shim injects secrets into the environment
and then REPLACES itself with the collector via os.execv, so the collector runs
under the pid the supervisor launched.

THREE GOTCHAS (each one bit the pilot's wrapper):

  1. os.execv, NOT subprocess. execv replaces this interpreter in place, so the
     collector inherits this pid; the supervisor's restart/stop signals and exit-code
     reads reach the collector. Do NOT use subprocess.Popen/call and then exit — that
     leaves the collector as a child/grandchild the supervisor cannot signal.

  2. Forward the supervisor's args VERBATIM (sys.argv[1:]). Do NOT hardcode
     `--config`; the supervisor owns the merged config file path and passes it in.

  3. Exit-code propagation is automatic with execv (the collector's exit status
     becomes this process's exit status). A wrapper that does
     `sys.exit(0)` after subprocess.call hides collector crashes and turns a bad
     config into a silent restart loop.
"""
import os
import sys

# (1) materialize secrets into the environment (replace with real vault reads).
#     Keep secrets in env only; never write them to the config file on disk.
os.environ.setdefault("SQUADRON_SECRET", "demo-secret-value")  # placeholder
# e.g. os.environ["SPLUNK_HEC_TOKEN"] = vault_read("secret/otel/splunk", "token")

# (2) exec the collector, forwarding the supervisor-supplied args verbatim.
#     sys.argv[1:] carries the supervisor's args (notably --config <merged>).
collector = os.environ.get("COLLECTOR_BIN", "/otelcol-contrib")
os.execv(collector, [collector] + sys.argv[1:])
# os.execv never returns on success; the collector's exit code becomes ours.
