# Security policy

## Reporting a vulnerability

Please report security issues privately, through
[GitHub private vulnerability reporting](https://github.com/geniuspath/apipod-cli/security/advisories/new).
Do not open a public issue for a vulnerability.

Include the CLI version (`apipod version`), your operating system, and the
steps to reproduce. Never include a real API key or token; `apipod auth status`
shows a masked value that is safe to share.

## Scope

In scope: anything that lets the CLI leak a credential, send a credential to a
host other than the one it was issued for, read or upload a file the user did
not name, write outside the output directory, spend balance past the spending
guard, or pass terminal control sequences from the server to the terminal.

The design and its limits are described in the
[Security section of the README](README.md#security).

## Supported versions

Security fixes are released for the latest version.
