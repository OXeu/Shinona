# Repository instructions

## Temporary files and directories

- Never write to `/tmp` or its descendants, including through tools that default to the system temporary directory.
- Store temporary files and directories in `.tmp/` at the workspace root. Create it as needed and set `TMPDIR` to its absolute path for tools that use it. Set `TMP` or `TEMP` to the same path for tools that use those variables.
- Before each use of `.tmp/`, remove files and directories inside it that have been inactive for more than two hours. Use the latest access or modification time and preserve entries still in use. Keep directories containing active entries. Restrict cleanup to `.tmp/` and avoid following symlinks outside it.
