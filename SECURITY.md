# Security Policy

## Supported Versions

I currently support the latest **major** release with security updates. I encourage users to stay up to date with the latest version.

| Version | Supported          |
| ------- | ------------------ |
| Latest  | :white_check_mark: |
| Older   | :x:                |

## Reporting a Vulnerability

I take the security of `thread-udp-proxy` seriously. If you discover a security vulnerability in this project, please follow these steps to report it:

1. **Do not open a public issue.** That could expose the vulnerability to malicious actors before a patch is available.
2. Report the vulnerability using **GitHub's private vulnerability reporting feature**:
   - Navigate to the **Security** tab of the repository.
   - Click on **Advisories** in the sidebar.
   - Click **Report a vulnerability**.
3. Provide a detailed description of the vulnerability, including:
   - A description of the issue.
   - Steps to reproduce it.
   - Potential impact.
   - Suggested mitigations (if any).

I aim to acknowledge your report within 48 hours and to provide an estimated timeline for addressing the issue. I will keep you updated on the progress and let you know when the fix is released.

## Security Best Practices

When deploying this sidecar, please consider the following:

- **Grant only `NET_RAW`:** the container needs `CAP_NET_RAW` for its raw packet socket and nothing else. Drop all other capabilities and keep the default nonroot user.
- **Scope what it relays:** set `PROXY_PREFIXES` to your Thread mesh's prefixes rather than relying on the broad `fc00::/7` default.
- **Internal networks only:** the infrastructure interface should be a trusted LAN segment. Anything that can send frames to the pod's MAC can send datagrams into the Thread mesh through it.
- **Keep the health server private:** bind `LISTEN_ADDR` to `127.0.0.1`, or restrict who can reach port `9090` with a network policy. It exposes no controls, but `/status` reveals the mesh's prefixes.
