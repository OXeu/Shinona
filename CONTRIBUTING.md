# Contributing

Keep account workflows in `internal/auth` and conversation behavior in `internal/run`. Transport, ORM, provider, and pi types belong in their adapters. Scope account data by the authenticated account ID. Agent commands go through `sandbox.Workspace.Exec`. Go lint enforces domain import boundaries and rejects `os/exec` in app code.

## Vue components and feedback

Compose PascalCase components in feature and app templates. Native HTML and SVG tags belong in `web/src/shared/ui`, where approved components implement semantics and accessibility. Vue slots and structural templates remain available for composition. ESLint rejects native tags, dynamic `component` tags, and direct Vue render-helper imports outside the UI directory.

Use `notify.success(message)` and `notify.error(message)` from `src/shared/notifications.ts` for transient app feedback. Chat errors appear in the transcript through `ChatSystemMessage`. Keep running and queued states in the composer and activity controls. The app mounts one Ding toaster. Use `UiChoiceDropdown` for form choices and `UiDialog` for confirmations or input. Native select, option, optgroup, alert, and dialog tags are banned in Vue templates. The shared `UiDialog` alone may render the native dialog to provide modal focus and Escape behavior.

Use Vue's conditional directives for optional content. ESLint reserves inline announcements such as `aria-live`, feedback roles, and `output` for the app toaster. Direct Ding imports outside the facade, browser dialogs, and raw HTML bindings are errors. HTTP requests use the shared HTTP client. CI treats ESLint warnings as failures and disables inline ESLint configuration. The feedback rule checks syntax. Review new text and interpolations for transient feedback that belongs in a toast.

The style lint rejects `outline-offset` in CSS and Vue styles and checks JavaScript and TypeScript style properties such as `outlineOffset`. The shared toaster renders Ding's state through Vue and uses the app's styles.

## Translations

Use `t` from `web/src/shared/i18n` for interface text, accessibility labels, and toast messages. Add new keys to all three catalogs and use named placeholders for variable text. Use the locale-aware time formatter in session views. The HTTP API returns stable error codes for the front end to translate. Catalog tests check for missing keys and inconsistent placeholders.

## Checks

Use `.js` and `.ts` for JavaScript and TypeScript source files. The lint command checks filenames across the repository and skips Git metadata, installed dependencies, temporary files in `.tmp/`, and generated output in `bin/` and `web/dist/`. Vue components use `.vue`.

Run from the project root.

```sh
golangci-lint run ./cmd/... ./internal/...
go run golang.org/x/tools/cmd/deadcode@v0.50.0 -test ./cmd/... ./internal/...
bash .github/scripts/report-go-structure.sh
go test -race ./cmd/... ./internal/...
npm --prefix web ci
npm --prefix web run lint
npm --prefix web run lint:unused
npm --prefix web run lint:structure
npm --prefix web test
npm --prefix web run build
```

CI blocks on unused code reported by Go lint, `deadcode`, or Knip. Go lint also enforces import boundaries and formatting. The reachability check includes tests on CI's Linux target. During a cleanup, run `deadcode` without `-test` to find functions reached only from tests, and check any other supported build targets before deleting a symbol. Review dynamic entry points and migration paths when investigating findings.

The structure reports flag Go and Web duplication, complex functions, lines over 140 characters, and source files over 400 lines. The Go report also flags interfaces with more than 10 methods. The Web report flags multiple statements on one line. Use the findings to review responsibilities and maintenance cost. Repository-wide line counts are an optional trend metric.

Coordinate REST route and response changes with the clients in `web/src/features/sessions/sessionApi.ts` and `web/src/features/sessions/settingsApi.ts`. Editable instance setting keys are registered in `internal/settings`. Protect credential fields with server-side redaction, and keep address and database configuration in startup wiring.

## Performance benchmarks

The [Benchmarks workflow](.github/workflows/benchmarks.yml) runs the core API, compression, load, SSH/Docker, and web benchmarks on pushes to `main`, pull requests targeting `main`, and manual runs. The `go-bench` artifact contains Go benchmark output, web measurements, and the combined Bencher Metric Format (BMF) JSON. Go benchmarks report `ns/op`, `B/op`, and `allocs/op`. The loopback HTTP benchmarks include response reads and client decoding where indicated. Account setup, fixture creation, and state resets run outside the timer.

| Benchmark | API and fixture |
| --- | --- |
| `SessionsList` | Authenticated `GET /api/v1/sessions`, returning 30 sessions from 256 across four projects |
| `SessionListPoll` | Full response, unchanged `304`, and a one-session rename delta against the same 256-session fixture. |
| `SessionsListFiltered` | Switches between four project filters and starts each request with an empty list snapshot history. |
| `WorkerScheduling` | Two 25 ms fixture turns in the same session or in separate accounts. `overlap-%` measures how many pairs execute concurrently. `start-us/op` measures the time from submission to the first turn starting. The same-session case verifies submission order. |
| `Transcript` | HTTP transcript read with Zstandard negotiation and Protobuf decoding, using the default latest 30 turns from a 120-turn session |
| `TranscriptServer` | Same transcript fixture through the authenticated HTTP handler with a counting response writer. It excludes loopback and client decoding and reports compressed response bytes as `wire-B/op`. |
| `TranscriptCold` / `TranscriptWithImages` | A historical cursor bypasses the SQLite latest-page cache. A fresh router clears response and encoded-turn caches. The image case verifies attachment names on turns with roughly 360 KiB PNGs stored separately from transcript JSON. |
| `TranscriptFullStream` | A 600-turn export exceeds the 512 KiB compressed-response cache limit. A probe verifies Protobuf completion and batch timing. Timed clients drain the compressed stream and the benchmark reports sampled `peak-heap-B` and `peak-disk-B`. |
| `Attachment` | A Cold download loads the image from SQLite and validates the decoded PNG. A Hot download reuses the cached bytes and checks turn visibility. |
| `AuthSession` | Authenticated lookups on a warm and an invalidated session cache. `InvalidateOwner` exercises 32 concurrent readers under the CI CPU setting while account updates invalidate their sessions. All three variants report p50 and p99. |
| `LiveSync` / `LiveStream` | Full and one-turn incremental reconnects to `/api/v1/live`, plus verified text and tool-log suffix deltas fanned out to 1, 8, and 32 subscribers. |
| `Send` / `Queue` | `POST /api/v1/sessions/{id}/turns` on an idle session and with an active turn, respectively. Timing ends at the submission response. `Queue` reports p50 and p99. |
| `Steering` | The REST steering endpoint targeting a held turn in the real worker. The fixture agent verifies delivery outside the timer. |
| `Revert` | The original revert operation through the REST endpoint, including the head check and returned draft. The fixture reverts three of 30 turns per request. |
| `GitChanges` / `FileList` / `GitHistory` | REST workspace reads on an already-running project sandbox with a 40-file Git fixture, 12 commits, and two pending changes. |
| `WorkspacePreview` / `WorkspaceCommit` | Verify a file truncated to 16 KiB and a commit of a selected change. Each commit iteration restores the benchmark repository's original HEAD outside the timer. |
| `Initialize` / `Recreate` / `Destroy` | REST project lifecycle operations. Initialize starts from an absent sandbox, and recreate and destroy start from a running sandbox. The SSH base image is already available on the host. |
| `PTYRoundTrip` | A binary input frame through the connected terminal WebSocket until the shell's response arrives. Sandbox, shell, and WebSocket startup are outside the timer. |
| `TerminalCreate` | Time from the create request to the first WebSocket status frame with `state: running`, reported as `ttfb-us/op`. |

Before running locally, follow the repository's [temporary file instructions](AGENTS.md).

```sh
mkdir -p .tmp
export TMPDIR="$PWD/.tmp" TMP="$PWD/.tmp" TEMP="$PWD/.tmp" GOTMPDIR="$PWD/.tmp"
go test -p 1 -cpu 2 -run '^$' -bench '^Benchmark(CoreAPI|ZstdTranscriptEncoding)$' \
  -benchmem -benchtime=1s -count=1 ./cmd/shinona ./internal/adapter/httpserver
```

Core load uses the same authenticated APIs. Readers warm up before each five-second measurement window, issue their next request after consuming the current response, and finish outstanding requests at the end of the window. The load generator and server share a Go process with `GOMAXPROCS=2` in CI.

| Benchmark | Bencher variant parameters |
| --- | --- |
| `CoreAPI/SessionsList` | `concurrency` of 1, 4, 16, or 64 crossed with `sessions` of 256 or 2,000. Each poll still returns the latest 30 sessions. |
| `CoreAPI/Transcript` | The same `concurrency` levels crossed with `page` of `latest` or `full`, with `turns` fixed at 200. The latest page returns 30 turns. The full response returns all 200. |
| `CoreAPI/Mixed` | 4, 16, or 64 list and latest-transcript readers, with two writers updating a title and advancing a read marker every 20 ms and 35 ms. The writers target separate sessions owned by the same account as the readers. |
| `CoreAPI/TranscriptFullStream` | 1 or 64 concurrent exports of the 600-turn fixture beyond the compressed cache limit. |

Load reports include mean, p50, and p99 latency in nanoseconds, completed requests per second, and wire bytes per read. `cache-hit-rate` measures hits in the HTTP transcript response cache. Mixed load also reports write latency and throughput and separate list and transcript read latencies. The test log records average `Server-Timing` phases, including stream trailers. CI saves the log and BMF v1 JSON in `core-read-load` and verifies variant parameters, measures, and percentiles before and after publishing.

The allocation, CPU, and GC pause totals are divided by completed requests, including writes in mixed load. They cover both client and server activity. `peak-heap` samples Go `HeapInuse` during load, and `heap-inuse` measures retained heap after two GCs. On Linux the benchmark measures process CPU with `getrusage` and resets `VmHWM` before sampling the process RSS high-water mark, which includes SQLite C allocations. Use `TranscriptServer` for handler-side allocation comparisons. The full streaming client drains bytes without building a decoded response in memory.

`disk-inuse` and `peak-disk` measure all regular files under the benchmark's data directory, including the SQLite database, WAL, shared-memory sidecar, and nested application files. The sampler runs at the beginning and end of each variant and every 250 ms during load. It also reports the final database size, peak WAL size, and final and peak application-file sizes. Linux reports allocated blocks and counts hard-linked files once. Other platforms report logical file lengths. The benchmark skips symlinks. Point future file stores at this same data directory to include their files in the totals.

```sh
GOMAXPROCS=2 SHINONA_BENCH_LOAD_OUTPUT="$PWD/.tmp/core-read-load.json" \
  go test -p 1 -count=1 -run '^TestCoreReadLoad$' -v ./cmd/shinona
GOMAXPROCS=2 SHINONA_BENCH_IDLE_WAL=1 \
  go test -p 1 -count=1 -run '^TestBenchmarkIdleWAL$' -v ./cmd/shinona
```

The opt-in idle WAL benchmark reports ten seconds of process CPU time and context switches. The CI codec benchmark compares Zstandard settings independently of HTTP. HTTP `wire-B/op` counts encoded response-body bytes and `body-B/op` counts decoded bytes. WebSocket wire metrics count server frames with framing and negotiated compression, excluding the HTTP upgrade.

The web benchmarks measure the entry point and statically imported JavaScript and CSS, plus the three emitted Arrow frame assets. The raw total includes the precompressed Wasm file as emitted. The gzip total uses its existing size and gzips the JS and HTML separately, matching the production limit of 480 KiB. The Node cases time decoding a 200-turn Protobuf response and grouping message parts from the decoded turns.

```sh
cd web
npm ci
npm run build -- --manifest
node bench/bundle-sizes.js
node --experimental-transform-types bench/session-paths.js
```

The SSH fixture runs a dedicated loopback OpenSSH server with a generated key and a Docker image containing Bash and Git. It needs `sshd`, `sudo -n`, and access to Docker. On Ubuntu and Debian, initialize the system OpenSSH runtime directory with `sudo install -d -o root -g root -m 0755 /run/sshd` if it is missing. Alternatively, use a dedicated SSH/Docker host by setting `SHINONA_BENCH_SSH_ADDRESS`, `SHINONA_BENCH_SSH_USER`, `SHINONA_BENCH_SSH_KEY_PATH`, `SHINONA_BENCH_SSH_HOST_FINGERPRINT`, and `SHINONA_BENCH_SSH_IMAGE`, then run the same Go command without the SSH fixture script.

```sh
bash .github/scripts/benchmark-ssh.sh go test -p 1 -cpu 2 -run '^$' \
  -bench '^BenchmarkSandboxAPI$/^ssh$' -benchmem -benchtime=5x -count=1 ./cmd/shinona
```

E2B runs against a dedicated benchmark account using `SHINONA_BENCH_E2B_API_KEY` and `SHINONA_BENCH_E2B_TEMPLATE`. The template needs Bash, Git, and the usual Linux file utilities. For self-hosted E2B, set `SHINONA_BENCH_E2B_API_URL` and, if needed, `SHINONA_BENCH_E2B_PROXY_URL` and `SHINONA_BENCH_E2B_DOMAIN`. Each sandbox case uses a new project ID, cleans up its own resources, and can incur provider charges.

```sh
go test -p 1 -cpu 2 -run '^$' -bench '^BenchmarkSandboxAPI$/^e2b$' \
  -benchmem -benchtime=5x -count=1 ./cmd/shinona
```

Set the `BENCHER_API_KEY` repository secret to a project key for [Shinona](https://bencher.dev/console/projects/shinona). CI registers custom measures with units and publishes core, SSH, and web results to `github-ubuntu-24.04-x64`. Same-repository PR reports use `pr-<number>` with the target branch as their start point. Fork PRs and runs without a key still produce the artifacts. Bencher alerts on increases of 5% for wire bytes, 15% for allocated bytes, 10% for allocation counts, and 3% for gzip size after five samples. Mean latency alerts use a t-test. Tail latency and CPU are observational on shared runners. The Arrow gzip budget and report validation are enforced by CI.

To run E2B in Actions, configure the `SHINONA_BENCH_E2B_API_KEY` repository secret and the `SHINONA_BENCH_E2B_TEMPLATE` repository variable, then dispatch the workflow on `main` with `e2b` enabled. It produces a `go-bench-e2b` artifact and publishes to the separate `github-ubuntu-24.04-x64-e2b` testbed.

## Session transfer schema

Transcript HTTP responses and live session WebSocket messages use [session_transfer.proto](internal/adapter/sessiontransfer/session_transfer.proto). Keep existing Protobuf field numbers stable. After changing the schema, regenerate both checked-in bindings. Install `protoc` and `protoc-gen-go` v1.36.11 on your `PATH`.

```sh
npm exec --yes --package=@bufbuild/protoc-gen-es@2.17.0 -- \
  protoc -I internal/adapter/sessiontransfer \
  --go_out=internal/adapter/sessiontransfer/sessionpb --go_opt=paths=source_relative \
  --es_out=web/src/features/sessions/wire --es_opt=target=ts \
  internal/adapter/sessiontransfer/session_transfer.proto
```
