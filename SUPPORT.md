# Support

Bedrock is a small open-source program that runs a machine. Support is
best effort, from the maintainer, in the repository's issues and
discussions.

## A good report

- `bedrock version`, which names the build and its commit, and the
  machine's operating system
- the exact command and flags, and the `--plan` output where there is one
- the manifest, with hostnames and names changed where they are private
- the receipt, from `bedrock history <operation-id>` or `--json` output,
  which carries secret names and never values
- what you expected and what happened

## Out of scope

- debugging private production machines, or recovering their data
- the logic of the apps bedrock runs
- live credentials of any kind in a public issue

Where you can, reduce a problem to a fixture app and a sanitised manifest
before filing; that is also what a fix is tested against.
