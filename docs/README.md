# Documentation

Reference documentation for **ipsupport-airllm**, a self-hosted, OIDC-governed
LLM gateway. Start with the project [README](../README.md) for the overview.

| Guide | What it covers |
|-------|----------------|
| [Getting started](getting-started.md) | Run the local mock, make a first API call, tour the console |
| [Configuration](configuration.md) | Every environment variable and runtime (hot-reloaded) setting |
| [API reference](api.md) | Every HTTP endpoint — data-plane, control-plane, admin, audit |
| [Architecture](architecture.md) | Components, request flow, planes, storage, concurrency |
| [DLP, capture & audit](dlp-capture-audit.md) | Secret/PII scanning, the capture store, the training flywheel, the audit trail |
| [Operations](operations.md) | Building, testing, migrations, deploy notes, security posture |

Design records live under [`superpowers/specs`](superpowers/specs/) and
[`superpowers/plans`](superpowers/plans/). The protocol-translation approach is
described in [`translation.md`](translation.md).

All documentation is English-only; the repository contains no secrets.
