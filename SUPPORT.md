# Support

Bedrock is an open-source deployment control plane. Community support is best
effort unless a maintainer states otherwise.

## Good Support Requests

Open an issue or discussion with:

- Bedrock version or commit
- operating system and Python version
- exact command and flags
- redacted manifest or fixture that reproduces the issue
- expected behavior and actual behavior
- relevant JSON output with secrets removed

## Out Of Scope For Public Support

- debugging private production hosts
- recovering private secrets, databases, or volumes
- diagnosing product-specific business logic
- handling live provider credentials in public issues
- maintaining old deployment layouts that are not represented by Bedrock
  manifests and fixtures

For live deployments, start with fixture reproduction first. If the issue only
appears with real infrastructure, reduce it to a sanitized manifest, redacted
receipt, or synthetic fixture before filing.
