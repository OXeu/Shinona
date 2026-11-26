# Repository instructions

## Temporary files and directories

- Never write to `/tmp` or its descendants, including through tools that default to the system temporary directory.
- Store temporary files and directories in `.tmp/` at the workspace root. Create it as needed and set `TMPDIR` to its absolute path for tools that use it. Set `TMP` or `TEMP` to the same path for tools that use those variables.
- Before each use of `.tmp/`, remove files and directories inside it that have been inactive for more than two hours. Use the latest access or modification time and preserve entries still in use. Keep directories containing active entries. Restrict cleanup to `.tmp/` and avoid following symlinks outside it.

## Commits

- Commit functional changes separately in English according to [Conventional Commits 1.0.0](docs/conventional-commits.md). Descriptions must be imperative and at most 72 characters. See [Commit rules](docs/commit-rules.md).

## Redundancy and refactoring

- Use repository-wide source line counts only to observe trends. Keep them out of acceptance criteria and optimization targets.
- Give each cleanup a structural goal. Remove verified dead code or obsolete compatibility, consolidate mechanisms with the same behavior, or remove pass-through layers that add no value. Verify callers, supported platforms, and migration requirements before deleting code.
- Preserve readable names, useful comments and spacing, error handling, input validation, type and redaction boundaries, concurrency safeguards, timeouts, and test assertions. Avoid packing statements onto one line or replacing clear control flow to improve a metric.
- Apply the Rule of Three before introducing shared abstractions for similar code. Look for at least three cases with the same responsibility that need to change together. Avoid using boolean mode flags or reflection to combine different behaviors.
- For dedicated redundancy refactors, keep behavior equivalent and explain the redundancy removed in each change. Run the tests, Go race checks, and lint checks in [Contributing](CONTRIBUTING.md). Have an independent review agent compare the diff for swallowed errors, weakened checks, reduced readability, and abstractions that increase coupling.
