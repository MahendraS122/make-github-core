# In-memory GitHub-compatible forge

The Node 22 entry point launches `bin/forge`, a static Linux amd64 binary written in Go. The forge binds an ephemeral port, registers with `MAKEFUN_ENV_URL`, and waits for the trial environment to signal completion. It does not invoke Git or use external services.

## Interfaces

- Git smart HTTP at `/{owner}/{repo}.git`: upload-pack v2 (and v0 fallback), receive-pack v0, shallow fetch, SHA-1 objects, OFS/REF delta decoding, and fast-forward enforcement.
- REST v3 under `/api/v3` or at the root: repositories, Git refs and objects, contents, issues, labels, comments, and pull requests with file diffs and merges.
- GraphQL at `/api/graphql`: repository, issue, and pull request queries and mutations, cursor pagination, and schema introspection for the supported fields.
- MCP JSON-RPC at `/mcp`: discovery, initialization, and six repository, issue, and pull request tools backed by the same data as REST.

Use `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` for API calls. The trial's unauthenticated `/healthz` probe is the only exception. Repository data is in memory and is discarded on exit. Commits and API timestamps use `MAKEFUN_TRIAL_NOW` (or a fixed epoch if absent). The default authenticated account is the trial viewer `adaiversen`; set `MAKEFUN_FORGE_LOGIN` to use a different account in a local deployment.

## Build and test

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 /usr/local/go/bin/go build -ldflags "-s -w" -o bin/forge ./go/cmd/forge
/usr/local/go/bin/go test ./go/...
node --experimental-strip-types main.ts
```

Running the launcher requires a trial or local stub exposing `/register` and `/done`. Set `MAKEFUN_DONE_URL` to override the completion endpoint.
