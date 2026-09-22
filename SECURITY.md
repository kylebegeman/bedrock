# Security Policy

## Supported Versions

Bedrock is pre-1.0 infrastructure software. Security fixes target the default
branch first. Tagged release support will be documented once public releases are
cut.

## Reporting A Vulnerability

Do not open a public issue for suspected vulnerabilities. Use GitHub private
vulnerability reporting when it is enabled for the repository. If it is not
enabled, open a minimal public issue asking the maintainer to enable private
security reporting, without including exploit details or sensitive data.

Please include:

- affected version, commit, or branch
- component or command involved
- impact and likely exploit path
- reproduction steps using fixture data where possible
- whether any secret, token, host, or production data may have been exposed

Bedrock treats secret exposure, command injection, unsafe restore behavior,
provider mutation bypasses, receipt redaction gaps, and deploy confirmation
bypasses as high-priority security issues.

## Secret Handling

Never submit real secrets, `.env` values, private keys, provider tokens, host
IPs, personal paths, or production runtime payloads. Use secret reference names,
fixture values, and `example.com` domains in public material.
