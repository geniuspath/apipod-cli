# Python packaging

`pip install apipod-cli` installs the same Go binary as every other channel
and puts an `apipod` command on `PATH`.

The distribution is named **`apipod-cli`**: the name `apipod` on PyPI belongs
to an unrelated project. That project also installs a command called `apipod`,
so the two cannot share one environment — install this one with `pipx` or
`uv tool`, which give it an environment of its own:

```bash
pipx install apipod-cli
uv tool install apipod-cli
```

## What is in a wheel

One wheel per platform, tagged `py3-none-<platform>`. Each contains exactly:

```
apipod_cli-<version>.data/scripts/apipod[.exe]   the binary
apipod_cli-<version>.dist-info/                  METADATA, WHEEL, RECORD, LICENSE
```

There is no Python code, no `setup.py` and no source distribution, so nothing
runs at install time: pip copies the binary into the environment's `bin`
directory and checks it against the hashes in `RECORD`.

## Building the wheels

```bash
goreleaser release --snapshot --clean --skip=sign,sbom   # produces dist/
python3 scripts/build_wheels.py                          # produces python/dist/*.whl
```

`scripts/build_wheels.py` uses only the standard library. Its output is
reproducible: file order, timestamps and permissions are fixed. The release
workflow uploads the wheels with PyPI Trusted Publishing (OIDC); no PyPI token
is stored in the repository.
