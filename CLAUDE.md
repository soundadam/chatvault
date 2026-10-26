# chatvault

Go CLI + Bubble Tea v2 TUI that merges WeChat (macOS, WeChat 4) messages into
one append-only SQLite vault and keeps Markdown/JSONL exports current.

- `make check` (gofmt, vet, test) before committing; `make ci` is what CI runs.
- Layout: `internal/wechat` (find account, snapshot, decrypt, read),
  `internal/sqlcipher`, `internal/vault` (schema, ingest, queries),
  `internal/parse` (message kinds), `internal/export`, `internal/pipeline`
  (sync stages), `internal/cli` (cobra commands and the TUI).
- Schema: never edit a released entry in `internal/vault/schema.go`; add a
  migration at the end. Changing how messages are read is a `parse.Version`
  bump, which rebuilds `parsed` and the exports. See `docs/data-model.md`.
- Never print database keys or the contents of `keys.json` / `state.json`.
- Test against a scratch vault (`--vault <dir>`), not `~/ChatVault`.
