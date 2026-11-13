# npm packaging

`npm install -g apipod` installs the same Go binary as every other channel.

| Package | Contents |
| --- | --- |
| `apipod` | A small launcher (`bin/apipod.js`) and `optionalDependencies` on the six platform packages. No binary, no install scripts. |
| `@apipod/cli-<os>-<cpu>` | The `apipod` binary for one platform, restricted with `os` and `cpu` so npm installs only the matching one. |

There is deliberately **no `postinstall` script** and no download at install
time. The binary a user runs is the tarball npm fetched from the registry and
checked against the lockfile integrity hash. `npm install --ignore-scripts`
works.

## Building the packages

The platform packages are generated, not checked in:

```bash
goreleaser release --snapshot --clean      # produces dist/
node scripts/build-npm.mjs                 # produces npm/dist/<package>/
```

`scripts/build-npm.mjs` reads `dist/artifacts.json`, copies each binary into
its platform package and stamps the release version into every `package.json`.
The release workflow publishes the result with npm Trusted Publishing (OIDC)
and provenance; no npm token is stored in the repository.
