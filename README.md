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

## Performance checks

The subprocess benchmark starts the forge with deterministic fake credentials and a local
`/register` and `/done` stub. It measures sequential HTTP/1.1 keep-alive reads and creates,
three rounds of 200 real receive-pack pushes (including timings for the first three), and
cold concurrent depth-one fetches of a 200-commit repository. Fetch packs are decoded and
their checksums verified outside the timed interval.

```sh
GOMAXPROCS=2 FORGE_PERF=1 FORGE_PERF_ENFORCE=1 go test ./go/cmd/forge -run '^TestPerformance$' -count=1 -v
go test -race ./go/...
go test ./go/cmd/forge -run '^$' -bench BenchmarkCachedIssueList -benchmem
```

`FORGE_TEST_BINARY=/absolute/path/to/forge` benchmarks an existing binary instead of the
test subprocess. To run the unmodified external SDK suite, set
`FORGE_CONFORMANCE_RUNNER=/absolute/path/to/sdk/run.ts` and run
`go test ./go/cmd/forge -run '^TestExternalConformance$' -count=1 -v`.
Set `FORGE_PERF=1` to include its performance workloads, and
`FORGE_CONFORMANCE_OUTPUT=/tmp/forge-conformance.json` to retain its JSON report.
The SDK must reside under a Node package with `"type":"module"`.

Issue and repository responses are encoded when changed. Issue-list pages are cached by
repository and raw query shape and invalidated by every issue/comment mutation. Concurrent
identical fetches share one pack build; immutable object compression is reused across fetch
shapes. Receive-pack inflates objects before acquiring the repository write lock, looking up
external delta bases with short read locks. HTTP read/write timeouts remain disabled,
keep-alive stays enabled, and the server caps headers at 64 KiB.
