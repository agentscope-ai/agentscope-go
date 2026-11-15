# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in agentscope-go, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email: **security@agentscope.io**

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and aim to provide a fix or mitigation within 7 days for critical issues.

## Supported Versions

| Version | Supported |
|---------|-----------|
| `main` | Yes. Receives fixes first |
| `v2.0.11` (latest tag) | Best effort, critical fixes only |
| `v2.0.10` and earlier | No. Upgrade using the [module migration notes](CHANGELOG.md#changed--repository-and-module-path-move) |

## Security Considerations

agentscope-go includes tools that can execute shell commands (`Bash` tool) and read/write files. When deploying agents in production:

- Use the **Permission Engine** to control tool access (prefer `Explore` or `Default` mode over `Bypass`)
- Use workspace sandboxing for untrusted tool execution: `DockerWorkspace`,
  `K8sWorkspace`, or `BubblewrapWorkspace` on Linux, and `AppleContainerWorkspace`
  on macOS. Cloud sandboxes (`E2BWorkspace`, `OpenSandboxWorkspace`,
  `DaytonaWorkspace`) move the trust boundary to the vendor, so verify their
  isolation guarantees yourself. No backend in this repo enforces network
  allowlists or resource limits; see
  [STABILITY.md, Open hardening work](STABILITY.md#open-hardening-work)
- Never expose the Agent Service HTTP endpoints without authentication
- Rotate API keys regularly and use `SecretStr` to prevent key leakage in logs
