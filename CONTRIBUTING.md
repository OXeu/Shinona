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

The [Benchmarks workflow](.github/workflows/benchmarks.yml) runs the core API baselines, core read load variants, and live SSH/Docker benchmarks on pushes to `main`, pull requests targeting `main`, and manual runs. The `go-bench` artifact contains the baseline and sandbox Go output. These benchmarks use one authenticated client over loopback and report mean latency in ns/op. Request timing includes reading and decoding the response. Account setup, fixture creation, and state reset between iterations are outside the timer.

| Benchmark | API and fixture |
| --- | --- |
| `SessionsList` | Authenticated `GET /api/v1/sessions`, returning 30 sessions from 256 across four projects |
| `SessionListPoll` | Full response, unchanged `304`, and a one-session rename delta against the same 256-session fixture. `body-B/op` measures the uncompressed response size. |
| `WorkerScheduling` | Two 25 ms fixture turns in the same session or in separate accounts. `overlap-%` measures how many pairs execute concurrently. `start-us/op` measures the time from submission to the first turn starting. The same-session case verifies submission order. |
| `Transcript` | HTTP transcript read with Zstandard negotiation and Protobuf decoding, using the default latest 30 turns from a 120-turn session |
| `TranscriptServer` | Same transcript fixture through the authenticated HTTP handler with a counting response writer. It excludes loopback and client decoding and reports compressed response bytes as `wire-B/op`. |
| `Send` / `Queue` | `POST /api/v1/sessions/{id}/turns` on an idle session and with an active turn, respectively. Timing ends at the submission response. |
| `Steering` | The REST steering endpoint targeting a held turn in the real worker. The fixture agent verifies delivery outside the timer. |
| `Revert` | The original revert operation through the REST endpoint, including the head check and returned draft. The fixture reverts three of 30 turns per request. |
| `GitChanges` / `FileList` / `GitHistory` | REST workspace reads on an already-running project sandbox with a 40-file Git fixture, 12 commits, and two pending changes |
| `Initialize` / `Recreate` / `Destroy` | REST project lifecycle operations. Initialize starts from an absent sandbox, and recreate and destroy start from a running sandbox. The SSH base image is already available on the host. |
| `PTYRoundTrip` | A binary input frame through the connected terminal WebSocket until the shell's response arrives. Sandbox, shell, and WebSocket startup are outside the timer. |

Before running locally, follow the repository's [temporary file instructions](AGENTS.md).

```sh
mkdir -p .tmp
export TMPDIR="$PWD/.tmp" TMP="$PWD/.tmp" TEMP="$PWD/.tmp" GOTMPDIR="$PWD/.tmp"
go test -p 1 -cpu 2 -run '^$' -bench '^BenchmarkCoreAPI$' \
  -benchmem -benchtime=500ms -count=1 ./cmd/shinona
```

Core read load uses the same authenticated APIs. Each client issues its next request as soon as its previous response has been read and decoded. Clients warm up before the five-second measurement window and finish outstanding requests after it. Transcript clients cycle through four sessions. The load generator and server share a Go process with `GOMAXPROCS=2` in CI.

| Benchmark | Bencher variant parameters |
| --- | --- |
| `CoreAPI/SessionsList` | `concurrency` of 1, 4, 16, or 64 crossed with `sessions` of 256 or 2,000. Each poll still returns the latest 30 sessions. |
| `CoreAPI/Transcript` | The same `concurrency` levels crossed with `page` of `latest` or `full`, with `turns` fixed at 200. The latest page returns 30 turns. The full response returns all 200. |

Load results include mean, p50, and p99 latency in nanoseconds, plus throughput in completed requests per second. CI saves the raw BMF v1 report and test log in the `core-read-load` artifact. The load report selects BMF v1 explicitly so Bencher can filter variants by parameters and plot both latency percentiles on the latency measure.

The `BenchmarkZstdTranscriptEncoding` codec benchmark in `internal/adapter/httpserver` compares Zstandard settings without HTTP or client decoding. The web production build limits the combined compressed Arrow frame assets to 480 KiB.

```sh
GOMAXPROCS=2 SHINONA_BENCH_LOAD_OUTPUT="$PWD/.tmp/core-read-load.json" \
  go test -p 1 -count=1 -run '^TestCoreReadLoad$' -v ./cmd/shinona
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

Set the `BENCHER_API_KEY` repository secret to a project key for [Shinona](https://bencher.dev/console/projects/shinona). Core and SSH results go to the `github-ubuntu-24.04-x64` testbed. Same-repository PR reports use `pr-<number>` with the target branch as their start point. Fork PRs and runs without a key still produce the Go artifact. To run E2B in Actions, configure the `SHINONA_BENCH_E2B_API_KEY` repository secret and the `SHINONA_BENCH_E2B_TEMPLATE` repository variable, then dispatch the workflow on `main` with `e2b` enabled. It produces a `go-bench-e2b` artifact and publishes to the separate `github-ubuntu-24.04-x64-e2b` testbed.

## Session transfer schema

Transcript HTTP responses and live session WebSocket messages use [session_transfer.proto](internal/adapter/sessiontransfer/session_transfer.proto). Keep existing Protobuf field numbers stable. After changing the schema, regenerate both checked-in bindings. Install `protoc` and `protoc-gen-go` v1.36.11 on your `PATH`.

```sh
npm exec --yes --package=@bufbuild/protoc-gen-es@2.17.0 -- \
  protoc -I internal/adapter/sessiontransfer \
  --go_out=internal/adapter/sessiontransfer/sessionpb --go_opt=paths=source_relative \
  --es_out=web/src/features/sessions/wire --es_opt=target=ts \
  internal/adapter/sessiontransfer/session_transfer.proto
```
