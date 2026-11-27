# Shinona guide

See the [English README](../README.md) or [简体中文 README](../README_zh.md) for a product overview.

## Run locally

Use Go 1.26.2 or later, a C compiler for SQLite, and Node.js 24. Node.js 22 is supported from 22.13 onward.

Create a password account and start the API from the project root. The account command reads the password from a hidden terminal prompt or from one line of stdin and prints the account ID.

```sh
go run ./cmd/shinona create-user you@example.test
go run ./cmd/shinona
```

In a second terminal, start the frontend.

```sh
npm --prefix web ci
npm --prefix web run dev
```

Open `http://127.0.0.1:5173`. Vite proxies `/auth`, `/api/v1/`, and the `/api/terminal/v1` terminal API and WebSocket to the Go API on `127.0.0.1:8780`. Shinona loads the project-root `.env` with godotenv. Exported variables take precedence over the file. Vite also reads `SHINONA_ADDR` and can use `SHINONA_API_ORIGIN` as an explicit development proxy target. [.env.example](../.env.example) lists the startup values.

### Preview on a trusted LAN

Start the API with `SHINONA_WEB_ORIGIN` set to the exact address other devices use, for example `http://192.168.1.20:5174`. Keep `SHINONA_ADDR` on loopback. Build the web app and serve the preview on all IPv4 interfaces.

```sh
npm --prefix web run build
npm --prefix web run preview -- --host 0.0.0.0 --port 5174 --strictPort
```

Open the configured origin from the LAN so sign-in callbacks and WebSockets use the same address. HTTP carries sign-in and session traffic unencrypted. Use HTTPS for access beyond a trusted LAN.

## Language

Shinona supports English (US), Simplified Chinese (China), and Traditional Chinese (Hong Kong). Change the language on the sign-in page or in **Settings**. A browser stores its selection locally. On a first visit, Shinona uses the first supported browser preference and falls back to US English. The locale applies to the interface, dates, and app notifications. Conversation text and configured names keep their original language.

## Settings

Set the listen address with `SHINONA_ADDR`, the SQLite path with `SHINONA_DATABASE`, and the browser origin with `SHINONA_WEB_ORIGIN` in the startup environment or `.env`. If upgrading from a release with a saved Web origin, copy its value to `SHINONA_WEB_ORIGIN` before restarting. Shinona removes retired settings from SQLite at startup.

Password and OAuth sign-in limits are per IPv4 address or IPv6 /64 network. If you run Shinona behind a reverse proxy, set `SHINONA_TRUSTED_PROXIES` to the proxy's CIDR range and have the proxy append the client address to `X-Forwarded-For`.

**Sandbox** and **Shinona SSO** configure the instance. These settings use saved SQLite overrides first, then the startup environment, then built-in defaults. **Use startup value** removes an individual override. Each account configures its own **Model** and **MCP servers**. Those endpoints use public network addresses by default. An operator can allow specific private model endpoints by listing their HTTPS origins in `SHINONA_TRUSTED_MODEL_ORIGINS`. After upgrading from shared settings, enter the model and MCP settings for each account. Save applies a validated change without restarting Shinona.

Grant an existing account access to instance settings by ID. Use the ID printed by `create-user` or returned by `GET /auth/session` for a signed-in account.

```sh
go run ./cmd/shinona grant-admin USER_ID
```

Use `revoke-admin USER_ID` to remove access. Administrators control which endpoints receive deployment credentials and which OpenID Connect issuer can sign users in. The API returns credential presence only, and saves credentials in SQLite in clear text. Restrict access to the database and its backups.

## Agent workspace

Drag the dividers or focus them and use the arrow keys to resize the navigation, session list, recent list, and workspace panel. The Recent list starts with five entries and excludes active and unread sessions. Opening a completed conversation marks the displayed turn as read. Use a session's menu to pin, rename, or remove it. Pinned sessions appear first within their group. Removing a session archives it once active work has finished.

Use the project selector to add a project or filter sessions. **All projects** includes recent sessions from every project in your account. Send a message from the welcome composer to create a session in the selected project, or in the default project when **All projects** is selected. Project names can contain up to 80 characters and must be unique within your account. Session search matches titles and project names.

Use the More menu beside a project to open **Manage sandbox**. The page shows the provider, the current status, and the sandbox ID when available. From there you can create, destroy, or recreate the workspace and set a provider and template for the project. E2B projects can also override their timeout and timeout action. Empty fields inherit the instance defaults. Changing the provider, effective template, or timeout action destroys the current workspace, and the next use provisions a new one. Destroying or recreating a sandbox also closes its terminal shells. If the project configuration or selected sandbox has changed since you loaded the page, review the refreshed status before confirming again. Conversation history stays in Shinona.

Removing a named project closes its terminals and destroys its retained sandbox before moving its sessions to the default project. Pending turns stop, and conversation history remains available. If sandbox cleanup fails, the project remains and its turns stay with it.

The workspace panel has **Changes & Commit**, **Git History**, and **Files**. Workspace inspection is available between turns. The agent or terminal provisions the project workspace on first use. Files supports folders and read-only UTF-8 previews. The Git panels use a repository at the selected workspace folder and support diffs, the latest 40 commits, and a local commit of selected paths. Unrelated staged changes stay staged.

In the **Files** tab, open a folder and choose **Set as working directory** to use that folder for **Changes & Commit** and **Git History**. The selection applies to all sessions in the project, including sessions in the account's default project. You can change it during a turn while the workspace is available. Changes, history, diffs, and commits stay inside the selected folder. Renames across the folder boundary appear as additions or deletions. **Files** continues to browse the whole workspace, and choosing the workspace root restores the default behavior.

If the project or working directory changes while you review changes, select the files again before committing. API clients include the `selection` returned by `GET /api/v1/sessions/{id}/workspace/changes` in the commit request. The server returns `409 WORKSPACE_SELECTION_CHANGED` if the selection is missing or stale.

Chat renders assistant Markdown in a virtualized transcript. Each activity group expands directly to its thinking and tool calls. On success, the last activity button displays the elapsed time. Tool calls start collapsed, including failures. Chat errors appear as system messages. The composer handles queue and running state.

Use **Revert message** under a user prompt once the session is idle. Confirm to remove that turn and all later turns from the transcript and future model context, and replace the composer draft with the selected prompt and images. Shinona retains the reverted records in SQLite and leaves workspace files and Git history in place.

## Authentication

Password login uses Argon2id and requires a password of 12–128 bytes. Login uses revocable, seven-day, HTTP-only cookies with SameSite Lax. An HTTPS `SHINONA_WEB_ORIGIN` enables secure cookies. HTTP origins are accepted on loopback or an explicitly configured private IP address. The REST API requires sign-in, scopes sessions and preferences by account ID, and checks the browser origin on writes.

`SHINONA_AUTH_PASSWORD_CONCURRENCY` controls how many password operations can run at once across sign-in and account changes. It defaults to 2 and accepts 1–64. Size it together with the Argon2 memory setting. `SHINONA_AUTH_RATE_LIMIT_ENTRIES` caps the number of recently used rate-limit buckets in each of three pools for password login, OAuth login, and account changes. The default is 8192 per pool and the accepted range is 1–262144. Login buckets use client addresses with IPv6 grouped by /64. Account changes use account IDs. Restart Shinona after changing either setting.

Configure OpenID Connect in **Settings → Shinona SSO** and register the callback with the issuer. The corresponding startup variables are listed below. Clear the issuer, client ID, and secret together to disable OAuth.

| Setting | Value |
| --- | --- |
| `SHINONA_WEB_ORIGIN` | Browser origin. The default is `http://127.0.0.1:5173`. |
| `SHINONA_OAUTH_ISSUER` | The provider's public HTTPS discovery issuer, such as `https://accounts.google.com`. |
| `SHINONA_OAUTH_CLIENT_ID`, `SHINONA_OAUTH_CLIENT_SECRET` | The provider's confidential web client credentials. |
| Callback URL | Append `/auth/oauth/callback` to `SHINONA_WEB_ORIGIN`. Open the app from that exact origin. |

OAuth uses the authorization-code flow with PKCE, state, and nonce checks. A verified issuer and subject identify each OAuth account. Matching email addresses on password and OAuth accounts remain separate. OAuth accounts are created on first sign-in. [OpenID Connect Go library](https://github.com/coreos/go-oidc)

### Account

Open **Account** from the avatar menu to edit the nickname or upload a PNG, JPEG, or WebP avatar up to 512 KiB. Resetting an uploaded avatar restores the OAuth profile picture when available. Each account can bind one email address for password sign-in.

To bind or change a sign-in email, enter the code from the new mailbox. The code lasts 10 minutes and allows five attempts. Changing credentials requires the current password or, for an account with only OAuth, a sign-in from the last 15 minutes. After the email is verified, an OAuth account can set its first password. Changing the sign-in email or password revokes other login sessions.

Configure **Settings → Email delivery** with a Resend API key and a sender address on a verified domain to enable email binding. The startup fallbacks are `SHINONA_RESEND_API_KEY` and `SHINONA_RESEND_FROM`. At startup, `SHINONA_RESEND_API_URL` can override the public HTTPS endpoint, and `SHINONA_RESEND_TIMEOUT_SECONDS` controls the delivery timeout (1–60 seconds, default 8).

To attach the configured OAuth provider to an existing password account, sign in with the password and choose **Link OAuth** under **Account → Sign-in and security**. Confirm the current password and complete the provider sign-in. An OAuth identity can belong to only one account. An existing account can disconnect OAuth after setting a password. Disconnecting revokes the account's other sign-ins.

### User management

Grant the first super administrator role with `shinona grant-admin USER_ID`. Super administrators can open **User management** from the avatar menu to search accounts, create password users, grant or revoke administrator access, and disable or re-enable other users. A disabled account loses its login sessions and keeps its projects and conversation history.

An administrator can permanently delete another account from the same menu. This removes its projects, conversations, settings, and sign-in credentials from Shinona and stops its active turn and terminal shells. Retained workspaces and snapshots can be cleaned up through their provider. When OAuth account creation is available, a deleted or disconnected OAuth identity can sign in again as a new account.

## Sandboxes

Shinona bounds workspace requests independently of the sandbox's idle lifetime. `SHINONA_WORKSPACE_OPERATION_TIMEOUT_SECONDS` controls workspace reads, edits, status checks, and terminal registration (default 15, range 1–600). `SHINONA_PROJECT_SANDBOX_TIMEOUT_SECONDS` controls project sandbox lifecycle requests, terminal startup through PTY open, and agent startup through sandbox acquisition (default 120, range 1–900).

`SHINONA_WORKSPACE_CLEANUP_TIMEOUT_SECONDS` controls the time allowed to release each workspace or terminal resource (default 5, range 1–60). E2B uses it to stop interrupted commands and PTYs, discard newly created sandboxes that a project did not retain, and clear bindings after remote sandbox deletion. SSH uses it to close an unresponsive transport after command or terminal cancellation. Restart Shinona to apply startup changes.

E2B and SSH share one sandbox per account and project across sessions and turns. Shinona uses E2B as the instance default when both providers are configured. A project keeps its assigned provider until you switch it from **Manage sandbox**. The optional **Terminal** panel sits below the chat. Drag the divider or focus it and use the arrow keys to resize. Each tab runs an independent interactive Bash shell in the project workspace. A conversation supports up to eight tabs, and an account supports up to 32. Shinona keeps shells detached when the panel is hidden or the browser disconnects.

Each browser remembers the active tab and panel visibility by conversation and restores up to 256 KiB of recent output per tab. Closing a tab ends its shell. Archiving a conversation closes its tabs. Tabs live in the Shinona process and depend on the current project sandbox.

Forward `/api/terminal/v1/` and `/api/terminal/v1` through the reverse proxy. Preserve the WebSocket upgrade and browser `Origin` for `/api/terminal/v1/connect`.

Agent turns require an OpenAI-compatible endpoint that serves streaming Chat Completions. The E2B template needs Bash for workspace shell access.

1. Enter the API root in **Settings → Model**, such as `https://api.openai.com/v1`.
2. Set the API key.
3. Use **Fetch models** and select the default model. Shinona requests `GET /models` under that base URL.

The model picker requires a model catalog. [OpenAI model listing reference](https://developers.openai.com/api/reference/resources/models/methods/list)

### Composer

- Attach PNG, JPEG, or WebP images using the picker, paste, or drag and drop. A turn accepts up to four images totaling 4 MiB. The endpoint and model must support images. User messages load their images separately from the transcript. Model requests include earlier images when the model accepts them.
- In **Options**, select the model, permissions, and thinking effort for the next turn. **Read only** exposes the sandbox read tool. **Workspace access** also exposes write, Bash, and tools from the selected MCP server. Thinking effort depends on endpoint support. The context indicator updates when the model endpoint reports token usage. It shows the latest measured context usage and the model's context limit when available.
- In **Options**, use **Set as default** to save those three choices to your account for newly opened composers. **Reset to defaults** restores the saved choices in the current composer. If a saved model disappears from the catalog, the composer uses the account's default model.
- A conversation restores the options and MCP servers from its latest submitted turn. The browser remembers each conversation's unsent selection for the signed-in account and restores it when the page reloads.
- During a turn, the composer button stops the run when the draft is empty and queues the draft when it has text or an image. Right-click the queue button or press **Shift+F10** to open the action menu. In **Settings → Preferences**, choose **Queue** or **Steer** for Enter during a run. Steer supplies text to pi at its next checkpoint. Stopping an active turn leaves its queued turns intact.
- Add, edit, and remove your remote servers in **Settings → MCP servers**. Enter a unique name, the Streamable HTTP endpoint URL, and choose **No authentication**, **Bearer token**, or **OAuth**. Each Shinona account can connect to an OAuth server from this page. The composer lets you enable multiple MCP servers. Each submission uses the current selections and exposes the selected servers’ remote tools with text results. [MCP transport reference](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/docs/protocol.md)

For OAuth, save the server and select **Sign in**. Shinona discovers the authorization server and uses authorization code with PKCE S256. If the server requires a pre-registered client, register `SHINONA_WEB_ORIGIN` followed by `/auth/mcp/callback` as the redirect URL. Enter the registered client ID in the MCP server settings and provide the client secret if the registration requires one. Leave the client ID empty to use a Client ID Metadata Document or Dynamic Client Registration when the authorization server supports it. The metadata document requires Shinona to be reachable over HTTPS.

Shinona saves each account's grant in SQLite and refreshes it when the provider issues a refresh token. **Disconnect** removes Shinona's saved grant. [MCP authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)

Each account can configure up to 16 MCP servers. Server URLs must omit embedded credentials, queries, and fragments. **Remove all** clears the current account's server list and its saved grants. MCP responses can contain up to 4 MiB per JSON response or server-sent event, and up to 16 MiB per event stream.

### E2B Cloud and self-hosted E2B

Configure the E2B API key in **Settings → Sandbox**. The E2B template defaults to `base`. New workspaces use the template's default user and directory. Set **Workspace user** or **Workspace directory** to override them. These settings also accept startup values from `SHINONA_E2B_WORKSPACE_USER` and `SHINONA_E2B_WORK_DIRECTORY`. Changes take effect on the next connection. Clearing an override restores the default without recreating the sandbox. Older sandboxes opened as `root` in `/root` retain that default for compatibility.

Outbound access follows the E2B project or self-hosted network configuration. Shinona requests private ingress to obtain a traffic access token on self-hosted runtimes that issue credentials for private sandbox URLs. [E2B network controls](https://e2b.dev/resources/run-ai-generated-code-safely)

Shinona saves the sandbox ID and its private data-plane tokens in SQLite for each account, project, and E2B control plane. Sessions without a named project use the account's default workspace. Configure the fallback active window and expiration action under **Settings → Sandbox** with **Timeout in seconds** and **On timeout**. Their startup values are `SHINONA_E2B_TIMEOUT` (300 seconds) and `SHINONA_E2B_ON_TIMEOUT` (`pause`). Each project can override either value. Shinona requests this window when connecting or creating a sandbox and renews it during turns and active terminal sessions. E2B runtime limits still apply. On expiration, **Pause** preserves the filesystem and memory, and **Kill** terminates the sandbox. The action is set when the sandbox is created. [E2B sandbox persistence](https://e2b.dev/docs/sandbox/persistence)

If a project still has a sandbox on a previous E2B control plane, Shinona keeps the project and its remaining binding when removal cannot finish. An operator can restore the previous connection. Select E2B in **Manage sandbox**, destroy the old sandbox, then retry project removal. Repeat the cleanup for each control plane that still has a binding.

Self-hosted deployments need to support the sandbox connect endpoint and auto-pause if **Pause** is selected. With **Pause**, when E2B reports that a saved sandbox is gone, the next turn tries to restore an existing snapshot. If none is usable or snapshot listing is unsupported, Shinona provisions a clean workspace from the configured template. With **Kill**, the next turn after expiration provisions directly from the template. Temporary outages and authentication errors leave the saved workspace association in place. [E2B snapshots](https://e2b.dev/docs/sandbox/snapshots)

For a self-hosted deployment, configure the endpoints as follows.

- Set **E2B base URL** (`SHINONA_E2B_BASE_URL`) to the control plane. Typical values are `https://api.<cluster-domain>` and `http://127.0.0.1:3000` for local development.
- **E2B API version** (`SHINONA_E2B_API_VERSION`) defaults to `auto`. It tries v2 first and can switch to v1 when the route is unavailable. Set `v1` or `v2` to use a specific version.
- Set the cluster domain separately for wildcard sandbox hosts. The adapter uses the sandbox domain returned by E2B and falls back to `SHINONA_E2B_DOMAIN` when configured.
- For a separate header-routed envd proxy, set `SHINONA_E2B_PROXY_URL`, for example `http://127.0.0.1:3002`.

Shinona authenticates data-plane calls with the sandbox credentials returned by E2B, including the traffic token for private ingress. The E2B SDK equivalents `E2B_API_URL`, `E2B_SANDBOX_URL`, and `E2B_DOMAIN` also work. [E2B self-hosted runtime configuration](https://github.com/e2b-dev/runtime/blob/main/DEV-LOCAL.md)

### SSH

Use a Linux SSH host with `sh`, `base64`, and a user that can run the Docker CLI. In **Settings → SSH servers**, add a name, host and port, SSH user, and verified SHA256 host key fingerprint. Paste a private key or select it from your device. Use a key without a passphrase. **Test connection** checks SSH authentication and access to Docker on a saved server or a draft. Shinona stores the key with the account and only returns the server details when you load settings. Choose a saved SSH server under the project's **Manage sandbox** menu. You can edit a server while projects use it. Future connections use the updated settings, and workspace files stay on the host where they were created. To remove a server, switch those projects to another provider first. If cleanup fails during a provider switch, you can continue after confirming that the previous workspace may remain on its original provider.

User-configured SSH hosts use public network addresses by default. An operator can allow exact private addresses through `SHINONA_TRUSTED_SSH_ADDRESSES`. An operator can also supply an optional shared SSH provider with the startup values in [.env.example](../.env.example).

The SSH provider keeps one container per account and project across runs and SSH connections. It creates new containers with UID and GID 65534, an owned home and workspace, all Linux capabilities dropped, and no-new-privileges enabled. The default limits are 2 CPUs, 4 GiB of memory with no additional swap, and 512 processes. Operators can override these settings for shared and personal SSH providers using the `SHINONA_SSH_CONTAINER_*` startup variables.

The default Docker image is `buildpack-deps:bookworm-scm` and the default working directory is `/workspace`. Custom images need `sh`, `sleep`, `setsid`, and standard file utilities. They should contain a user entry for the configured UID if tools need to resolve its name. Terminals use Bash when installed and fall back to `sh`. SSH must allow two session channels on a terminal connection so its watchdog can stop jobs in the terminal's process session when the connection closes. The repository panel requires Git.

Shinona applies resource limits to existing containers when they next run. Their original user and capabilities remain in effect until the project sandbox is recreated. Export any workspace files you want to retain before using **Recreate sandbox**.

## Session transfer

The session list uses `GET /api/v1/sessions`. Clients can send `If-None-Match` for an unchanged `304` response. Add `delta=true` with a previously received ETag in that header to request changes. When the requested base has left the cache, the server returns the current full list. The browser pauses polling when the tab is hidden. Visible tabs showing the same account and project filter share requests when the browser supports Shared Workers. When the last waiting tab cancels its refresh, the worker cancels the shared request.

`GET /api/v1/sessions/{id}/transcript` returns a Protobuf `TranscriptPage` with media type `application/protobuf`. A page returns up to 30 turns and a cursor for fetching earlier turns with `beforeSequence`. Pages and paged live updates group turns using a 4 MiB budget for stored message data. A turn larger than the budget can be returned on its own.

With `paged=false`, the response uses `application/vnd.shinona.transcript-stream+protobuf`. Each unsigned varint length is followed by one `TranscriptExportFrame`. The stream starts with a header, delivers the turns in sequence, and finishes with an end frame whose `failure` is `NONE` on success. An end frame with `LIMIT`, `TIMEOUT`, or `INTERNAL` reports an export that stopped after streaming began. EOF without an end frame means the transfer was interrupted. Errors before streaming starts use the normal JSON error response with an HTTP status.

The full export allows up to 5,000 turns, 128 MiB before compression, and 45 seconds including time spent waiting for an export slot. Use pagination for larger histories. The server applies Zstandard to JSON and Protobuf responses of at least 1 KiB when the client accepts `zstd`.

Set `SHINONA_TRANSCRIPT_EXPORT_CONCURRENCY` (1–32) and `SHINONA_TRANSCRIPT_PAGE_CONCURRENCY` (1–128) to tune the two request limits independently. The CPU-scaled defaults range from 1–4 full exports and 8–32 paginated requests. Restart Shinona after changing either value.

`SHINONA_SQLITE_READ_CONNECTIONS` controls the read-only connection pool for file-backed SQLite databases independently of the HTTP concurrency limits. It accepts 1–64 and defaults to the Go process's CPU concurrency, bounded to 2–16. The file database has a separate single writer. In-memory databases share one connection for reads and writes. Restart Shinona to apply the change.

These startup settings control retained session list and transcript data. Each accepts 0–512 MiB, with 0 disabling that cache. The byte budgets account for cached payloads. Entry-count and per-entry limits still apply, and process memory also includes indexing and in-flight responses.

| Setting | Cached data | Default |
| --- | --- | ---: |
| `SHINONA_SESSION_LIST_CACHE_MIB` | Session list snapshots and their encoded responses | 16 MiB |
| `SHINONA_SQLITE_TRANSCRIPT_CACHE_MIB` | Decoded latest pages from SQLite | 32 MiB |
| `SHINONA_TRANSCRIPT_TURN_CACHE_MIB` | Encoded Protobuf turns | 32 MiB |
| `SHINONA_TRANSCRIPT_RESPONSE_CACHE_MIB` | Encoded HTTP pages and small exports | 16 MiB |

An open conversation in a visible browser tab receives Protobuf updates over `/api/v1/live`. Client and server negotiate WebSocket compression when they connect. Growing text and tool output are sent as appended text. Changes to the turn list or status include changed turn records. A hidden tab closes its live connection and pauses automatic transcript refreshes. The browser reconciles with the current transcript when the tab becomes visible again or the connection resumes. It polls about once per second while a visible active session's live socket is unavailable. Requests for transcript pages and read markers time out after 15 seconds. Intermediate progress can coalesce for slow viewers. The server validates the login on connection and at least every 15 seconds while delivering updates or heartbeats. Login changes from a separate process can take up to two additional seconds to expire from the HTTP authentication cache.

The server caps each live message at 64 MiB of uncompressed Protobuf. Legacy full-history subscriptions are limited to 256 turns and 64 MiB of stored data for turns the client needs. If a full-history sync exceeds these limits, the server closes the connection with code 1009 and the client can reconnect with a paged subscription.

On a reverse proxy, forward `/api/v1/` to Shinona and preserve the WebSocket upgrade and browser `Origin` for `/api/v1/live`.

## Architecture and limits

API responses include `Server-Timing` metrics in milliseconds. `app` measures elapsed time before the Go server starts the response. Repeated phases are summed. Phases can be nested or run in parallel, so their durations can overlap.

Firefox displays phase durations in the Network Timings panel in a secure context. Use HTTPS for a LAN preview, or loopback HTTP when viewing the preview on the same host. A plain HTTP LAN preview still shows the raw `Server-Timing` value under Response Headers.

| API | Business phases |
| --- | --- |
| Sign-in and OAuth | Session lookup, password lookup and verification on successful sign-in, session creation, OIDC token exchange and verification, MCP resource and issuer discovery, registration, and grant persistence. |
| REST | Model discovery reports upstream HTTP, catalog decoding, normalization, and caller wait time. Workspace operations report sandbox acquisition, command execution, file reads, and release when used. |
| SQLite-backed APIs | `sqlite` sums GORM statement durations, including row scanning, across the request. |
| Transcript and attachments | Transcript admission wait, retrieval, stored turn JSON decoding (`transcript_json`), cache hits, and JSON encoding. Attachments report retrieval and image decoding. |
| Live sessions and terminals | The WebSocket handshake includes completed request phases. When a connection closes, the server logs aggregate phase durations and counts for bootstrap, resync, live deltas, encoding, and socket writes as applicable. Terminal startup logs sandbox acquisition and PTY allocation when they finish, and terminal connections log input, resize, and output writes at close. |

| Layer | Responsibility |
| --- | --- |
| [cmd/shinona](../cmd/shinona) | Configuration, account CLI, HTTP server, and adapter wiring. |
| [internal/auth](../internal/auth) | Sign-in, credential storage contracts, and account identities. |
| [internal/run](../internal/run) | Conversations, stored turns, per-account settings, and worker lifecycle. |
| [internal/settings](../internal/settings) | Editable instance settings, environment fallbacks, partial overrides, and credential redaction. |
| [internal/sandbox](../internal/sandbox) | Provider capabilities, workspace acquisition, file operations, process execution, optional interactive terminals, and release. |
| [internal/adapter](../internal/adapter) | Gin REST, HTTP auth, OIDC, GORM/SQLite, sky-valley/pi, E2B, and the E2B data-plane client. |
| [web/src](../web/src) | Vue Router, Pinia, REST clients, UI components, and Ding notifications. |

The API uses one Go process and one worker. A turn begins with one user submission and can include multiple model requests and tool calls. Each conversation can have up to eight pending turns, including one running turn. A running turn can receive up to 16 steering messages. The queue survives restart and interrupted runs become failed.

Shinona shows current activity and completed steps as Pi runs. The completed steps remain available in the conversation. Preview text and tool output can use up to 8 KiB each, and tool input can use up to 4 KiB.

Pi prepares the conversation for the selected model and compacts long histories when the model's context window is known. SQLite saves Pi's compacted context alongside the turn. Later requests resume from the latest applicable compacted context and replay the messages after it. When another compaction is needed, Pi updates the saved summary with newer history. Reverted turns and submissions cancelled before processing stay out of model history. The conversation list returns the newest 30 sessions.

The worker runs until the agent finishes or errors, the user stops it, or the server shuts down. A model request fails after three minutes without progress. For event streams, new text, reasoning, tool call activity, or a higher completion-token count resets the idle timer. For JSON completions, incoming non-whitespace data resets it. Each request accepts up to 32 MiB of event-stream data, 8 MiB of JSON completion data, or 64 KiB of HTTP error details. If a stream exceeds its limit, text already received stays in the conversation. Model endpoints enforce their own context windows, output defaults, and quotas.

Tools accept an optional integer `timeout` in seconds from the tool call. Without one, the tool inherits turn cancellation. MCP tools with a native timeout parameter use their own schema. Tool errors are sent to the assistant with any captured output. Workspace cleanup and result persistence keep separate deadlines.

SQLite stores conversations, thinking previews, and tool previews. Prompts are limited to 8,000 Unicode code points, image attachments to four images totaling 4 MiB per submission, and REST request bodies to 6 MiB. The initial workspace tools are text read, text write, and Bash. File tool operations are capped at 64 KiB and Bash output at 16 KiB per stream.

The API listens on loopback. The skeleton uses Vite for frontend development and has process-level sign-in throttling. Production serving, distributed rate limiting, provider quotas, and multi-worker coordination are outside this pass.

Existing pre-login runs stay private until an operator assigns them. Stop the server, use the same database, and run `go run ./cmd/shinona adopt-legacy USER_ID`. Each old run becomes its own conversation. Old unfinished runs are imported as failed. The command preserves completed prompts and results.

Password and conversation flows have browser coverage. Tests exercise signed OpenID Connect responses, automatic MCP client registration and token refresh, and local HTTP fixtures for both E2B endpoints.

Shinona uses the [Apache License 2.0](../LICENSE). See [Contributing](../CONTRIBUTING.md) for lint policies and local checks.
