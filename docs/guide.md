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

Open `http://127.0.0.1:5173`. Vite proxies `/auth`, `/graphql`, `/api/session-transfer/v1/`, and the `/api/terminal/v1` terminal API and WebSocket to the Go API on `127.0.0.1:8780`. Shinona loads the project-root `.env` with godotenv. Exported variables take precedence over the file. Vite also reads `SHINONA_ADDR` and can use `SHINONA_API_ORIGIN` as an explicit development proxy target. [.env.example](../.env.example) lists the startup values.

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

**Sandbox** and **Shinona SSO** configure the instance. These settings use saved SQLite overrides first, then the startup environment, then built-in defaults. **Use startup value** removes an individual override. Each account configures its own **Model** and **MCP servers**. Those endpoints use public network addresses by default. An operator can allow specific private model endpoints by listing their HTTPS origins in `SHINONA_TRUSTED_MODEL_ORIGINS`. After upgrading from shared settings, enter the model and MCP settings for each account. Save applies a validated change without restarting Shinona.

Grant an existing account access to instance settings by ID. Use the ID printed by `create-user` or returned by `GET /auth/session` for a signed-in account.

```sh
go run ./cmd/shinona grant-admin USER_ID
```

Use `revoke-admin USER_ID` to remove access. Administrators control which endpoints receive deployment credentials and which OpenID Connect issuer can sign users in. The API returns credential presence only, and saves credentials in SQLite in clear text. Restrict access to the database and its backups.

## Agent workspace

Drag the dividers or focus them and use the arrow keys to resize the navigation, session list, recent list, and workspace panel. The Recent list starts with five entries and excludes active and unread sessions. Opening a completed conversation marks the displayed turn as read. Use a session's menu to pin, rename, or remove it. Pinned sessions appear first within their group. Removing a session archives it once active work has finished.

Use the project selector to add a project or filter sessions. **All projects** includes recent sessions from every project in your account. Send a message from the welcome composer to create a session in the selected project, or in the default project when **All projects** is selected. Project names can contain up to 80 characters and must be unique within your account. Session search matches titles and project names.

Use the menu next to a saved project to rename or remove it. Removing a project moves its sessions into the default project.

The workspace panel has **Changes & Commit**, **Git History**, and **Files**. Workspace inspection is available between turns. The agent or terminal provisions the project workspace on first use. Files supports folders and read-only UTF-8 previews. The Git panels use a repository at the selected workspace folder and support diffs, the latest 40 commits, and a local commit of selected paths. Unrelated staged changes stay staged.

In the **Files** tab, open a folder and choose **Set as working directory** at the end of the path to use that folder for **Changes & Commit** and **Git History**. The button hides while the open folder is already the working directory. The selection applies to the current conversation and is stored in SQLite. Changes, history, diffs, and commits then stay inside the selected folder; a rename that crosses its boundary shows as an addition or deletion on this side. **Files** continues to browse the whole workspace so any folder can be selected, and choosing the workspace root again restores the default behavior.

Chat renders assistant Markdown in a virtualized transcript. Each activity group expands directly to its thinking and tool calls. On success, the last activity button displays the elapsed time. Tool calls start collapsed, including failures. Chat errors appear as system messages. The composer handles queue and running state.

Use **Revert message** under a user prompt once the session is idle. Confirm to remove that turn and all later turns from the transcript and future model context, and replace the composer draft with the selected prompt and images. Shinona retains the reverted records in SQLite and leaves workspace files and Git history in place.

## Authentication

Password login uses Argon2id and requires a password of 12–128 bytes. Login uses revocable, seven-day, HTTP-only cookies with SameSite Lax. An HTTPS `SHINONA_WEB_ORIGIN` enables secure cookies. HTTP origins are accepted on loopback or an explicitly configured private IP address. The API requires sign-in for GraphQL, scopes sessions and preferences by account ID, and requires same-origin browser requests for JSON mutations.

Configure OpenID Connect in **Settings → Shinona SSO** and register the callback with the issuer. The corresponding startup variables are listed below. Clear the issuer, client ID, and secret together to disable OAuth.

| Setting | Value |
| --- | --- |
| `SHINONA_WEB_ORIGIN` | Browser origin. The default is `http://127.0.0.1:5173`. |
| `SHINONA_OAUTH_ISSUER` | The provider's discovery issuer, such as `https://accounts.google.com`. |
| `SHINONA_OAUTH_CLIENT_ID`, `SHINONA_OAUTH_CLIENT_SECRET` | The provider's confidential web client credentials. |
| Callback URL | Append `/auth/oauth/callback` to `SHINONA_WEB_ORIGIN`. Open the app from that exact origin. |

OAuth uses the authorization-code flow with PKCE, state, and nonce checks. A verified issuer and subject identify each OAuth account. Matching email addresses on password and OAuth accounts remain separate. OAuth accounts are created on first sign-in. [OpenID Connect Go library](https://github.com/coreos/go-oidc)

### Account

Open **Account** from the avatar menu to edit the nickname or upload a PNG, JPEG, or WebP avatar up to 512 KiB. Resetting an uploaded avatar restores the OAuth profile picture when available. Each account can bind one email address for password sign-in.

To bind or change a sign-in email, enter the code from the new mailbox. The code lasts 10 minutes and allows five attempts. Changing credentials requires the current password or, for an account with only OAuth, a sign-in from the last 15 minutes. After the email is verified, an OAuth account can set its first password. Changing the sign-in email or password revokes other login sessions.

Configure **Settings → Email delivery** with a Resend API key and a sender address on a verified domain to enable email binding. The startup fallbacks are `SHINONA_RESEND_API_KEY` and `SHINONA_RESEND_FROM`.

To attach the configured OAuth provider to an existing password account, sign in with the password and choose **Link OAuth** under **Account → Sign-in and security**. Confirm the current password and complete the provider sign-in. An OAuth identity can belong to only one account. An existing account can disconnect OAuth after setting a password. Disconnecting revokes the account's other sign-ins.

### User management

Grant the first super administrator role with `shinona grant-admin USER_ID`. Super administrators can open **User management** from the avatar menu to search accounts, create password users, grant or revoke administrator access, and disable or re-enable other users. A disabled account loses its login sessions and keeps its projects and conversation history.

An administrator can permanently delete another account from the same menu. This removes its projects, conversations, settings, and sign-in credentials from Shinona and stops its active turn and terminal shells. Sandboxes and snapshots retained by E2B are managed on E2B. When OAuth account creation is available, a deleted or disconnected OAuth identity can sign in again as a new account.

## Sandboxes

E2B shares one sandbox per account and project across sessions and turns. The optional **Terminal** panel sits below the chat. Drag the divider or focus it and use the arrow keys to resize. Each tab runs an independent interactive Bash shell in the project workspace. A conversation supports up to eight tabs, and an account supports up to 32. Shinona keeps shells detached when the panel is hidden or the browser disconnects.

Each browser remembers the active tab and panel visibility by conversation and restores up to 256 KiB of recent output per tab. Closing a tab ends its shell. Archiving a conversation closes its tabs. Tabs live in the Shinona process and depend on the current E2B sandbox.

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
- During a turn, the composer button stops the run when the draft is empty and queues the draft when it has text or an image. Right-click the queue button or press **Shift+F10** to open the action menu. In **Settings → Preferences**, choose **Queue** or **Steer** for Enter during a run. Steer supplies text to pi-go at its next checkpoint. Stopping an active turn leaves its queued turns intact.
- Add, edit, and remove your remote servers in **Settings → MCP servers**. Enter a unique name, the Streamable HTTP endpoint URL, and choose **No authentication**, **Bearer token**, or **OAuth**. Each Shinona account can connect to an OAuth server from this page. The composer lets you enable multiple MCP servers. Each submission uses the current selections and exposes the selected servers’ remote tools with text results. [MCP transport reference](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/docs/protocol.md)

For OAuth, save the server and select **Sign in**. Shinona discovers the authorization server and uses authorization code with PKCE S256. Shinona generates the callback from `SHINONA_WEB_ORIGIN` and uses a Client ID Metadata Document when the authorization server advertises support and can reach Shinona over HTTPS. For servers that advertise Dynamic Client Registration, Shinona can register automatically. Authorization servers that require a pre-registered client cannot start a new connection from this interface.

Shinona saves each account's grant in SQLite and refreshes it when the provider issues a refresh token. **Disconnect** removes Shinona's saved grant. [MCP authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)

Each account can configure up to 16 MCP servers. Server URLs must omit embedded credentials, queries, and fragments. **Remove all** clears the current account's server list and its saved grants.

### E2B Cloud and self-hosted E2B

Configure the E2B API key in **Settings → Sandbox**. The E2B template defaults to `base`. The template must contain the `user` account and `/home/user`. Outbound access follows the E2B project or self-hosted network configuration. Shinona requests private ingress to obtain a traffic access token on self-hosted runtimes that issue credentials for private sandbox URLs. [E2B network controls](https://e2b.dev/resources/run-ai-generated-code-safely)

Shinona saves the sandbox ID and its private data-plane tokens in SQLite for each account, project, and E2B control plane. Sessions without a named project use the account's default workspace. Every turn or workspace inspection reconnects to the saved ID and requests a five-minute active window. During a turn or while the terminal is open, Shinona renews the window once a minute, subject to the provider's runtime limits. When the window expires, E2B pauses the sandbox with its filesystem and memory preserved. [E2B sandbox persistence](https://e2b.dev/docs/sandbox/persistence)

Self-hosted deployments need to support auto-pause and the sandbox connect endpoint. If E2B reports a saved sandbox is gone, the next turn tries to restore an existing snapshot of that sandbox. If no usable snapshot exists or snapshot listing is unsupported, Shinona provisions a clean workspace from the configured template. Recovery uses snapshots already stored by E2B and can only restore files captured in them. Temporary outages and authentication errors leave the saved workspace association in place. [E2B snapshots](https://e2b.dev/docs/sandbox/snapshots)

Removing a project in Shinona leaves its sandbox in E2B for manual cleanup. The project's sessions then use the default project workspace. Changing the template applies to newly provisioned sandboxes.

For a self-hosted deployment, configure the endpoints as follows.

- Set **E2B base URL** (`SHINONA_E2B_BASE_URL`) to the control plane. Typical values are `https://api.<cluster-domain>` and `http://127.0.0.1:3000` for local development. This selects the self-hosted sandbox creation endpoint.
- Set the cluster domain separately for wildcard sandbox hosts. The adapter uses the sandbox domain returned by E2B and falls back to `SHINONA_E2B_DOMAIN` when configured.
- For a separate header-routed envd proxy, set `SHINONA_E2B_PROXY_URL`, for example `http://127.0.0.1:3002`.

Shinona authenticates data-plane calls with the sandbox credentials returned by E2B, including the traffic token for private ingress. The E2B SDK equivalents `E2B_API_URL`, `E2B_SANDBOX_URL`, and `E2B_DOMAIN` also work. [E2B self-hosted runtime configuration](https://github.com/e2b-dev/runtime/blob/main/DEV-LOCAL.md)

## Session transfer

Shinona reads the session list as a Brotli-compressed Protobuf page over a reusable WebSocket. It reads transcripts over HTTP as length-prefixed, Zstandard-compressed Protobuf frames with up to eight turns each. The browser applies a transcript after its completion frame arrives. Both use the existing authenticated read service. The transcript includes all visible turns in the session. GraphQL handles writes and provides compatible reads when the binary route is unavailable.

An open conversation receives Zstandard-compressed Protobuf updates over a WebSocket. Growing text and tool output are sent as appended text. Changes to the turn list or status include changed turn records. The browser reconciles with the current transcript on reconnect and polls about once per second while an active session's live socket is unavailable. Intermediate progress can coalesce for slow viewers. The server sends a heartbeat every 15 seconds and checks the login cookie when it sends an update.

The browser loads the decoders on first use. The session-list socket closes after 45 seconds of server-side idle time. A failed binary poll falls back to GraphQL and waits 30 seconds before trying the socket again. The transcript reader rejects incomplete frames and individual compressed frames over 64 MiB.

On a reverse proxy, forward `/api/session-transfer/v1/` to Shinona. Preserve the WebSocket upgrade and browser `Origin` for `/api/session-transfer/v1/recent` and `/api/session-transfer/v1/live`. Stream `/api/session-transfer/v1/transcript` with response buffering disabled if the proxy buffers HTTP responses by default.

## Architecture and limits

API responses include `Server-Timing` metrics in milliseconds. `app` measures elapsed time before the Go server starts the response. Repeated phases are summed. Phases can be nested or run in parallel, so their durations can overlap.

Firefox displays phase durations in the Network Timings panel in a secure context. Use HTTPS for a LAN preview, or loopback HTTP when viewing the preview on the same host. A plain HTTP LAN preview still shows the raw `Server-Timing` value under Response Headers.

| API | Business phases |
| --- | --- |
| Sign-in and OAuth | Session lookup, password lookup and verification on successful sign-in, session creation, OIDC token exchange and verification, MCP resource and issuer discovery, registration, and grant persistence. |
| GraphQL | JSON decoding and encoding, validation, execution, and each executed root field. Model discovery reports upstream HTTP, catalog decoding, normalization, and caller wait time. Workspace operations report sandbox acquisition, command execution, file reads, and release when used. |
| SQLite-backed APIs | `sqlite` sums GORM statement durations, including row scanning, across the request. |
| Transcript and attachments | Transcript request decoding, transcript and settings retrieval, and first-frame encoding. Attachments report retrieval and image decoding. The transcript's final `Server-Timing` trailer reports frame assembly, Protobuf encoding, Zstandard compression, and socket writes across the stream. Vite forwards this trailer. Use an HTTP client that reads trailers to inspect the full stream. |
| Live sessions and terminals | The WebSocket handshake includes completed request phases. When a connection closes, the server logs aggregate phase durations and counts for session-list queries, bootstrap, resync, live deltas, encoding, compression, and socket writes as applicable. Terminal startup logs sandbox acquisition and PTY allocation when they finish, and terminal connections log input, resize, and output writes at close. |

| Layer | Responsibility |
| --- | --- |
| [cmd/shinona](../cmd/shinona) | Configuration, account CLI, HTTP server, and adapter wiring. |
| [internal/auth](../internal/auth) | Sign-in, credential storage contracts, and account identities. |
| [internal/run](../internal/run) | Conversations, stored turns, per-account settings, and worker lifecycle. |
| [internal/settings](../internal/settings) | Editable instance settings, environment fallbacks, partial overrides, and credential redaction. |
| [internal/sandbox](../internal/sandbox) | Provider capabilities, workspace acquisition, file operations, process execution, optional interactive terminals, and release. |
| [internal/adapter](../internal/adapter) | GraphQL, HTTP auth, OIDC, GORM/SQLite, pi-go, E2B, and the E2B data-plane client. |
| [web/src](../web/src) | Vue Router, Pinia, GraphQL operations, UI components, and Ding notifications. |

The API uses one Go process and one worker. A turn begins with one user submission and can include multiple model requests and tool calls. Each conversation can have up to eight pending turns, including one running turn. A running turn can receive up to 16 steering messages. The queue survives restart and interrupted runs become failed.

Shinona keeps a live preview and stores up to 48 thinking, intermediate text, or tool segments per turn. Preview text and tool output can use up to 8 KiB each, and tool input can use up to 4 KiB. These preview limits apply to the recorded trace.

Model requests carry the prompts, images the model supports, delivered steering, and saved final answers from all earlier processed turns in the same session. Reverted turns and submissions cancelled before processing stay out of model history. The conversation list returns the newest 30 sessions. The transcript loads the entire session.

The worker runs until the agent finishes or errors, the user stops it, or the server shuts down. Shinona sends model requests without a per-call deadline or token cap. Model endpoints still enforce their context windows, output defaults, and quotas.

Tools accept an optional integer `timeout` in seconds from the tool call. Without one, the tool inherits turn cancellation. MCP tools with a native timeout parameter use their own schema. Tool errors are sent to the assistant with any captured output. Workspace cleanup and result persistence keep separate deadlines.

SQLite stores conversations, thinking previews, and tool previews. Prompts are limited to 8,000 Unicode code points, image attachments to four images totaling 4 MiB per submission, and GraphQL request bodies to 6 MiB. The initial workspace tools are text read, text write, and Bash. File tool operations are capped at 64 KiB and Bash output at 16 KiB per stream.

The API listens on loopback. The skeleton uses Vite for frontend development and has process-level sign-in throttling. Production serving, distributed rate limiting, provider quotas, and multi-worker coordination are outside this pass.

Existing pre-login runs stay private until an operator assigns them. Stop the server, use the same database, and run `go run ./cmd/shinona adopt-legacy USER_ID`. Each old run becomes its own conversation. Old unfinished runs are imported as failed. The command preserves completed prompts and results.

Password and conversation flows have browser coverage. Tests exercise signed OpenID Connect responses, automatic MCP client registration and token refresh, and local HTTP fixtures for both E2B endpoints.

Shinona uses the [Apache License 2.0](../LICENSE). See [Contributing](../CONTRIBUTING.md) for lint policies and local checks.
