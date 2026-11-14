# apipod

[简体中文](README.zh-CN.md)

The command-line client for [APIPod](https://www.apipod.ai): generate images and
video, estimate what a generation costs, and upload reference media, from a
terminal, a script or an agent.

```bash
apipod auth login
apipod image "a red fox in the snow" -m <model> --aspect 16:9 -o ./out
```

Every command works for people (tables, progress on stderr) and for programs:
add `--json` for one JSON document on stdout, and branch on the
[exit code](#exit-codes).

## Install

All channels install the same Go binary.

```bash
brew install --cask geniuspath/tap/apipod
```

```bash
npm install -g apipod
```

```bash
pipx install apipod-cli
```

Or download an archive from the
[releases page](https://github.com/geniuspath/apipod-cli/releases) and
[verify it](#verifying-a-download).

Notes:

- **npm** — the binary comes from a platform package
  (`@apipod/cli-<os>-<cpu>`) installed through `optionalDependencies`. No
  install script runs and nothing is downloaded at install time, so
  `npm install --ignore-scripts` works.
- **pip** — the package is named `apipod-cli`. The name `apipod` on PyPI
  belongs to an unrelated project that also installs a command called
  `apipod`; use `pipx` or `uv tool install apipod-cli` so this CLI gets an
  environment of its own.
- **Go** — `go install github.com/geniuspath/apipod-cli/cmd/apipod@latest`
  (Go 1.24 or newer).

Shell completion: `apipod completion bash|zsh|fish|powershell` (the Homebrew
cask installs it for you).

## Sign in

```bash
apipod auth login
```

This opens the browser, asks you to approve the "APIPod CLI" application, and
stores the resulting tokens in the system keychain. On a machine without a
browser, the sign-in URL is printed so you can open it elsewhere; the browser
must be able to reach `127.0.0.1` on the machine running the CLI.

To use an API key instead:

```bash
export APIPOD_API_KEY=...        # for scripts and CI
apipod auth login --with-api-key # stores a key in the keychain; reads stdin or a hidden prompt
```

There is deliberately no `--api-key` flag: command-line arguments end up in
shell history and are visible to other processes.

```bash
apipod auth status   # which credential is in use (the secret is masked)
apipod auth logout   # revokes the tokens, then removes them from this machine
```

`APIPOD_API_KEY` takes precedence over a stored credential.

## Examples

```bash
# Find a model and see what it accepts
apipod models list --category video
apipod models params <model>

# What will it cost?
apipod cost <model> -d duration=10 -d resolution=1080p

# Generate an image, wait, and download it into ./out
apipod image "a red fox in the snow" -m <model> --aspect 16:9 -o ./out

# Edit a local image (it is uploaded first)
apipod image "make it night" -m <model> --image ./photo.png

# Video from a first frame, with model-specific parameters
apipod video "she turns and smiles" -m <model> --image ./frame.png \
  --duration 5 --resolution 1080p -x generate_audio=true

# Submit without waiting, then pick the job up later
apipod video "a drone shot over a fjord" -m <model> --no-wait --json
apipod job wait <job-id> -o ./out

# Upload media and get URLs back
apipod upload ./photo.png ./clip.mp4 --json

apipod balance
```

### Model parameters

Parameters without a dedicated flag go in `-x key=value`. Names are lowercase
with underscores, exactly as `models params` prints them. A value that is
valid JSON is sent as JSON (`-x seed=42`, `-x draft=true`,
`-x camera='{"zoom":1.5}'`); anything else is sent as a string.
`-x key=@./file` uploads a local file and sends its URL. Dedicated flags win
over `-x`.

`--image`, `--video` and `--audio` fill `image_urls`, `video_urls` and
`audio_urls`. Some models name an input differently — `models params` shows
the exact name — and take it through `-x`:

```bash
apipod video "extend the shot" -m <model> -x video_url=@./clip.mp4
```

### For scripts and agents

```bash
job=$(apipod video "a sunrise" -m <model> --no-wait --max-cost 2 --json | jq -r .job_id)
apipod job wait "$job" -o ./out --json | jq -r '.results[].path'
```

- `--json` prints exactly one JSON document on stdout. Progress and warnings go
  to stderr. On failure stdout stays empty and stderr carries
  `{"error": {"message", "exit_code", "code", "http_status", "job_id"}}`.
- `--max-cost <usd>` is a hard cap; `--yes` approves spending without a prompt.
  Without a terminal one of the two is required above the confirmation
  threshold (see [Spending guard](#spending-guard)).
- `--no-wait` returns the job id immediately; `--timeout` bounds a wait
  (exit code 7, and the job keeps running).
- `--quiet` silences progress; `--no-color` disables colors (also `NO_COLOR`).

## Commands

| Command | Purpose |
| --- | --- |
| `auth login` / `logout` / `status` | Sign in through the browser or store an API key; revoke; inspect |
| `models list [--category image\|video\|speech]` | Browse the catalog |
| `models params <model>` | Parameters a model accepts |
| `cost <model> [-d k=v] [--has-video]` | Price quote, with the current balance |
| `image <prompt> -m <model> [--image] [--aspect] [-n] [-x k=v]` | Generate or edit an image |
| `video [prompt] -m <model> [--image] [--video] [--audio] [--duration] [--resolution] [--aspect] [-x k=v]` | Generate a video |
| `job get <id>` / `job wait <id> [-o dir] [--timeout]` | Check a job; wait and download |
| `upload <file>...` | Upload local media, print public URLs |
| `balance [--open]` | Remaining balance; `--open` opens the billing page |
| `config get` / `set` / `list` | Local settings |
| `version`, `completion` | Version; shell completion scripts |

Global flags: `--json`, `--quiet`, `--no-color`, `--debug`, `--no-wait`,
`-o/--output <dir>`, `--max-cost <usd>`, `--yes`.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | General error (including a job that failed) |
| 2 | Usage error |
| 3 | Authentication: not signed in, or the credential was rejected |
| 4 | Rate limited |
| 5 | Not found (model, job) |
| 6 | Billing: insufficient balance, over `--max-cost`, or spending not confirmed |
| 7 | Timeout |
| 8 | Validation: the request or a local file was rejected |

### Configuration

Settings live in `config.toml` in the user configuration directory
(`apipod config list` prints the path). Credentials are never stored there.

| Key | Meaning |
| --- | --- |
| `defaults.image_model`, `defaults.video_model` | Model used when `-m` is omitted |
| `defaults.output` | `human` (default) or `json` |
| `spend.confirm_above` | Estimated cost in USD above which a generation needs approval (default `1`) |

| Environment variable | Meaning |
| --- | --- |
| `APIPOD_API_KEY` | API key; takes precedence over a stored credential |
| `APIPOD_BASE_URL` | API origin (default `https://api.apipod.ai`); must be `https`, except loopback |
| `APIPOD_CONSOLE_URL` | Web console origin, for self-hosted setups |
| `APIPOD_CONFIG_DIR` | Directory for `config.toml` and the fallback credential file |
| `NO_COLOR` | Disable colors |

## Security

The CLI is meant to be safe to hand to an agent, including one that is being
fed hostile instructions. This section states what it does and does not do.

### Credentials

- **Storage.** Tokens and keys go to the system keychain (macOS Keychain,
  Windows Credential Manager, Secret Service on Linux). Only when no keychain
  is available are they written to `credentials.json` with mode `0600`, with a
  warning. That file is refused if group or others can access it, or if it is
  a symbolic link.
- **No secrets in arguments.** No flag accepts a key or token. If one is typed
  in the wrong place anyway — as a flag value, a command or a parameter name —
  the usage error does not repeat it.
- **Bound to a host.** A credential is stored under the API origin that issued
  it and is only ever sent there. Pointing `APIPOD_BASE_URL` at another host
  does not send a stored credential to it. The API client does not follow
  redirects.
- **HTTPS only.** A custom base URL must be `https`; plain `http` is accepted
  for `127.0.0.1`, `::1` and `localhost` only.
- **Not printed.** `auth status` shows a masked value. With `--debug`,
  `Authorization` and similar headers are replaced with `[REDACTED]`, known
  secrets are removed from every logged line, and OAuth request bodies are not
  logged at all.
- **An explicit environment key is yours to direct.** `APIPOD_API_KEY` is sent
  to whatever `APIPOD_BASE_URL` names. If you override the base URL, you are
  choosing where that key goes.

### Sign-in

Authorization code flow with PKCE (S256) for the public client `apipod-cli`.
The callback listener binds `127.0.0.1` on a random port, accepts one callback,
compares `state` in constant time, times out after five minutes, and closes as
soon as the callback arrives. The callback page is static and echoes nothing
from the request. Access tokens last an hour and are renewed with a rotating
refresh token. `auth logout` revokes both tokens on the server and removes
them locally; if revocation fails, the local copy is still removed and you are
told to revoke the authorization in the console.

### Spending guard

Before a generation the CLI asks the API for a price estimate.

- At or below `spend.confirm_above` (default $1): it proceeds.
- Above it, in a terminal: it asks.
- Above it, without a terminal: it **refuses with exit code 6** unless `--yes`
  or `--max-cost` was given.
- `--max-cost <usd>` is a cap in every case, and `--yes` does not override it.
  It must be a finite, non-negative number.
- If the price cannot be estimated, the request is treated as needing
  approval; with `--max-cost` it is refused.

The check runs before any reference file is uploaded and before the task is
created.

The estimate is computed from the same parameters that are sent. Parameter
names given with `-x` and `-d` must be lowercase (`n`, not `N`): the API reads
request fields case-insensitively but prices them by exact name, so a
differently-cased name would be billed without being estimated. A reference
video is reported to the estimate whichever parameter carries it. A
`callback_url` is never forwarded; the CLI polls for the result.

The guard limits what one command can spend. It does not limit how many
commands are run: for a hard ceiling, give the agent an API key with a quota
set in the console.

Every generation request carries an `Idempotency-Key`, reused for each retry
of that request, so a retry is never billed as a second task. Only connection
errors and HTTP 429, 502 and 503 are retried.

### Local files

- Only files named explicitly are read. Directories, symbolic links, devices
  and FIFOs are refused; there is no globbing or recursion; at most 20 files
  per command.
- The first 3 KB are sniffed and only PNG, JPEG, WebP, GIF, MP3, WAV, M4A, MP4,
  WebM and MOV are accepted, whatever the extension. `apipod upload ~/.ssh/id_rsa`
  or `apipod upload .env` is refused locally. Size limits match the server:
  images 20 MB, audio 50 MB, video 100 MB.
- All files of a command are checked before anything is sent: one refused file
  means no request is made.
- Only the file's base name is sent, not its path.
- **Uploaded files get public URLs.** Anyone who has the URL can open the file.
  Do not upload anything you would not publish.

### Downloads

Results are fetched with a separate HTTP client that never sends your
credential, over `https` only (redirects included). File names are generated by
the CLI as `<job-id>-<n>.<ext>`, with the extension taken from the content, so
the server cannot choose a path. Existing files are not overwritten without
`--force`. Files are written to a temporary name and renamed into place, with a
2 GiB limit per file.

### Terminal output

Text that comes from the server (model names, error messages, job fields) has
ANSI/OSC escape sequences, control characters and bidirectional overrides
removed before it is printed. `--json` output is escaped by the JSON encoder.

### What the CLI does not do

It makes no payments (`balance --open` opens the console), accepts no password,
sends no telemetry, and does not check for updates.

### Verifying a download

Release archives are listed in `checksums.txt`, which is signed with
[cosign](https://github.com/sigstore/cosign) using the release workflow's
identity. No signing key exists to be stolen.

```bash
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/geniuspath/apipod-cli/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

```bash
sha256sum --check --ignore-missing checksums.txt
```

Each archive also has a GitHub build provenance attestation and an SPDX SBOM:

```bash
gh attestation verify apipod_<version>_<os>_<arch>.tar.gz --repo geniuspath/apipod-cli
```

npm packages are published with provenance (`npm audit signatures`), and PyPI
wheels through Trusted Publishing.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Development

```bash
go test ./...
go vet ./...
go run ./cmd/apipod --help
```

Against a local backend:

```bash
APIPOD_BASE_URL=http://127.0.0.1:8080 go run ./cmd/apipod auth login
```

Release dry run (needs [GoReleaser](https://goreleaser.com) v2.16 or newer):

```bash
goreleaser check
```

```bash
goreleaser release --snapshot --clean --skip=sign,sbom
```

Drop `--skip=sign,sbom` when `cosign` and `syft` are installed. Then
`node scripts/build-npm.mjs` and `python3 scripts/build_wheels.py` assemble the
npm packages and wheels from `dist/`; see [npm/README.md](npm/README.md) and
[python/README.md](python/README.md).

## License

[MIT](LICENSE)
