# nvelox-ingress-controller — docs

Operator-facing documentation. Source of truth for what this controller does, how to install it, and what to do when it misbehaves.

| Doc | When you need it |
|---|---|
| [install.md](install.md)                     | Day-1 install via Helm + `make install`, including HA + upgrade |
| [architecture.md](architecture.md)           | How the controller + nvelox sidecar fit together, reload mechanism, failure modes |
| [ingress-mapping.md](ingress-mapping.md)     | Reference: what every `Ingress` field becomes in nvelox YAML |
| [troubleshooting.md](troubleshooting.md)     | Symptom → cause → fix for the common breakages |
| [roadmap.md](roadmap.md)                     | What's shipped, what's queued, what's deferred |

Also useful:

* [../samples/](../samples/) — applyable example manifests for the common patterns
* [../README.md](../README.md) — short project overview
