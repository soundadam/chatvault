# Data model

The vault is one SQLite file, `vault.sqlite`, in three layers. Each layer has one
writer, so any layer above the first can be thrown away and rebuilt.

| layer | tables | written by | may be rebuilt |
|---|---|---|---|
| 1 imported | `sources`, `runs`, `messages`, `revisions`, `contacts`, `contact_history` | `sync` (merge) | never: only added to |
| 2 derived | `chats`, `parsed` | `sync` (parse) | yes, from layer 1 |
| 3 user | `chat_prefs`, `exports`, `export_files` | `chat set`, `export add`, export runs | no |

The tables and their columns are commented in
[internal/vault/schema.go](../internal/vault/schema.go).

## Messages and revisions

A message's identity is `(source_id, chat, msg_key)`. `msg_key` is
`s:<server_id>` when WeChat gave the message a server ID, else
`l:<time>:<local_type>:<first 8 bytes of sha256(content)>`.

The first form a message is seen in stays in `messages`. When a later sync
sees the same key with different type or content (a recall, an edit, a
status change) the new form goes into `revisions`, keyed by its hash, so the
same change is recorded once. A revision of type 10000 marks the message as
recalled; the original text is still there.

Shards are merged oldest first, so the oldest form of a message wins.

`content` is kept as WeChat stored it (possibly zstd). Nothing in layer 1 is
interpreted, so a better parser can always go back to the source.

## Parsing

`parse.Version` is written into every `parsed` row. When it goes up, `sync`
reparses every row older than it, and export targets rebuild in full. A new
message kind or a fix to an existing one is a parser change, not a schema
change.

## Exports

An export target is a row in `exports`: a format, a directory and a config
(whether hidden chats are included, which chat kinds). Every file a target writes is recorded in
`export_files`. An export run:

1. rebuilds everything when the target is new, its directory or config changed,
   or the parser version moved;
2. otherwise rewrites every month of a chat whose prefs or contact changed
   since `prefs_at`, and the months that gained messages or revisions since
   `run_id`;
3. deletes files it no longer produces and empty folders.

Files are written to a temporary name and renamed, so a reader never sees half
a file.

## Changing the schema

- `migrations[i]` takes the vault from `user_version` i to i+1. A released
  migration is never edited; a change is a new entry at the end.
- Prefer adding columns and tables over changing them. Layer 2 can instead be
  dropped and rebuilt by bumping `parse.Version`.
- A vault newer than the binary is refused, rather than written to.
