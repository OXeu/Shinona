# Contributing

Keep account workflows in `internal/auth` and conversation behavior in `internal/run`. Transport, ORM, provider, and pi types belong in their adapters. Scope account data by the authenticated account ID. Agent commands go through `sandbox.Workspace.Exec`. Go lint enforces domain import boundaries and rejects `os/exec` in app code.

## Vue components and feedback

Compose PascalCase components in feature and app templates. Native HTML and SVG tags belong in `web/src/shared/ui`, where approved components implement semantics and accessibility. Vue slots and structural templates remain available for composition. ESLint rejects native tags, dynamic `component` tags, and direct Vue render-helper imports outside the UI directory.

Use `notify.success(message)` and `notify.error(message)` from `src/shared/notifications.ts` for transient app feedback. Chat errors appear in the transcript through `ChatSystemMessage`. Keep running and queued states in the composer and activity controls. The app mounts one Ding toaster. Use `UiChoiceDropdown` for form choices and `UiDialog` for confirmations or input. Native select, option, optgroup, alert, and dialog tags are banned in Vue templates. The shared `UiDialog` alone may render the native dialog to provide modal focus and Escape behavior.

ESLint rejects `v-if`, `v-else-if`, `v-else`, and `v-show`, along with common inline announcement patterns, including `aria-live`, literal feedback roles, and the `output` element. Direct Ding imports outside the facade, browser dialogs, and raw HTML bindings are errors. HTTP requests use the shared HTTP client. CI treats ESLint warnings as failures and disables inline ESLint configuration. The feedback rule checks syntax. Review new text and interpolations for transient feedback that belongs in a toast.

The style lint rejects `outline-offset` in CSS and Vue styles and checks JavaScript and TypeScript style properties such as `outlineOffset`. Vite removes these declarations from Ding’s embedded stylesheet in development and production.

## Translations

Use `t` from `web/src/shared/i18n` for interface text, accessibility labels, and toast messages. Add new keys to all three catalogs and use named placeholders for variable text. Use the locale-aware time formatter in session views. HTTP authentication and GraphQL return stable error codes for the front end to translate. Catalog tests check for missing keys and inconsistent placeholders.

## Checks

Use `.js` and `.ts` for JavaScript and TypeScript source files. The lint command checks filenames across the repository and skips Git metadata, installed dependencies, and generated output in `bin/` and `web/dist/`. Vue components use `.vue`.

Run from the project root.

```sh
golangci-lint run ./cmd/... ./internal/...
go test -race ./cmd/... ./internal/...
npm --prefix web ci
npm --prefix web run lint
npm --prefix web test
npm --prefix web run build
```

Coordinate GraphQL schema changes with the hand-written operations and types in `web/src/features/sessions/sessionApi.ts` and `web/src/features/sessions/settingsApi.ts`. Editable instance setting keys are registered in `internal/settings`. Protect credential fields with server-side redaction, and keep address and database configuration in startup wiring.

## Session transfer schema

The binary read adapter uses [session_transfer.proto](internal/adapter/sessiontransfer/session_transfer.proto). Keep existing Protobuf field numbers stable. After changing the schema, regenerate both checked-in bindings. Install `protoc` and `protoc-gen-go` v1.36.11 on your `PATH`.

```sh
npm exec --yes --package=@bufbuild/protoc-gen-es@2.17.0 -- \
  protoc -I internal/adapter/sessiontransfer \
  --go_out=internal/adapter/sessiontransfer/sessionpb --go_opt=paths=source_relative \
  --es_out=web/src/features/sessions/wire --es_opt=target=ts \
  internal/adapter/sessiontransfer/session_transfer.proto
```
