# Security policy

LiveGuard is a local reference implementation for studying policy-gated Agent execution. It is not a hardened multi-tenant service.

## Reporting a vulnerability

Use GitHub private vulnerability reporting when it is enabled for the repository. If it is unavailable, open a public issue titled `Security contact request` without technical details; the maintainer should provide a private channel before reproduction information is shared. Do not publish credentials, working exploits, private URLs, browser profiles, or user data in a public issue.

Include the affected revision, reproduction steps, expected impact, and any suggested mitigation. Reports about container escape, command injection, SSRF, credential exposure, authorization bypass, or cross-run data access are especially useful.

## Security boundaries

- The HTTP control plane and gRPC Sandbox Worker bind to loopback addresses by default and do not implement production authentication or TLS.
- The model can propose only the server-defined `diagnose_live_page` tool. It cannot provide a shell command, container image, mount, network mode, or Docker argument.
- The Policy Gateway independently validates the requested tool, user objective, browser evidence, reason length, and human-intervention state.
- Sandbox processes run as a non-root user with a read-only root filesystem, no network, dropped Linux capabilities, `no-new-privileges`, and CPU, memory, PID, output, and time limits.
- Docker containers share the host kernel. This is container isolation, not a microVM, gVisor, or Kata security boundary. Do not run arbitrary untrusted code with this project.
- The Sandbox Worker can access the local Docker daemon. Keep it on a trusted development machine and do not expose its gRPC port publicly.
- Browser targets default to `douyin.com`, `www.douyin.com`, `localhost`, and `127.0.0.1`; an entry matches either the exact hostname or its subdomains, so `douyin.com` also permits `live.douyin.com`. `LIVEGUARD_ALLOWED_HOSTS` overrides that comma-separated list. Redirect and DNS behavior are not hardened against SSRF or rebinding and must be treated as untrusted in any internet-facing deployment.

## Secrets and local data

Use `.env.local` for local secrets. `.env`, `.env.local`, `state/`, `runtime/`, `artifacts/`, and `.cache/` are ignored by Git. Before publishing a fork, scan its complete Git history as well as the working tree.

Screenshots and task records may contain page content. Review generated artifacts before sharing them.
