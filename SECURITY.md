# Security

LiveGuard is a local reference project, not a hardened internet-facing service.

## Boundaries

- The local HTTP server binds to loopback by default and has no user authentication or TLS.
- The model can request only server-registered monitoring tools. It cannot pass arbitrary shell commands, filesystem paths, or browser actions.
- Chrome uses a dedicated local profile. Do not publish that profile or screenshots that may contain private information.
- Browser host allowlists reduce accidental access to unrelated sites, but this project is not hardened against every redirect, DNS rebinding, or SSRF scenario.

## Local data and credentials

Keep API credentials in local environment configuration. The launcher may read an existing local MiniMax provider configuration, but it must not print or commit the key. Git ignores `.env.local`, `runtime/`, `artifacts/`, `state/`, and `.cache/`.

For vulnerability reports, use the repository's private GitHub reporting form. Include the affected revision, reproduction steps, impact, and any suggested mitigation. Do not publish credentials, private URLs, browser profiles, or user data.
