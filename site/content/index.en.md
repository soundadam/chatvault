---
title: chatvault
excerpt: Keep every message from WeChat for Mac in one local vault that only grows, exported per chat and month as Markdown and JSON Lines, rewriting only what changed.
weight: 55
presentation:
  category: WeChat history / CLI + TUI · macOS
  label: Install with Homebrew
  command:
    - brew trust --tap soundadam/tap
    - brew install --cask soundadam/tap/chatvault
  note: Free, Apache-2.0. Everything stays on your Mac. No upload, no telemetry.
registry:
  github: soundadam/chatvault
  homebrew: soundadam/tap/chatvault
  docs: https://github.com/soundadam/chatvault#readme
release:
  version: "0.1.0"
  license: Apache-2.0
modules:
  - type: promo
    description: WeChat keeps your history on one device, and a new phone, a cleanup or a recalled message each take a piece of it. {title} copies and decrypts the WeChat 4 for Mac databases, merges them into one SQLite vault in `~/Documents/ChatVault`, and writes each chat's month as a Markdown and a JSON Lines file you can search, back up or feed to other tools.
    line: "{title} {version} is a free {license} command-line tool for Apple silicon and Intel Macs. No account needed."
    actions:
      - label: Docs
        url: "{docs}"
      - label: GitHub
        url: "https://github.com/{github}"

  - type: snapshot
    title: Why chatvault
    body: WeChat's own backup moves a whole device and gives you nothing you can read. Most export tools dump everything once, and next month you start over. {title} works the other way round. The vault only grows, and the exports follow it. The first sync of 480,000 messages takes about 25 seconds; after that a sync usually takes about a second and rewrites only the months that gained messages.
    note: "Before the first sync, import the databases' keys with `chatvault keys import keys.json`. {title} does not extract them; the README names the tool that does, and `chatvault check` says what is still missing. WeChat 4.x for Mac only. The decrypted copies stay in the vault folder on your Mac, so keep that folder out of cloud drives and repositories."
    metrics:
      - value: "~1 s"
        label: A sync with nothing new
      - value: "0"
        label: Originals lost to a recall; the recall is kept beside them
      - value: "4"
        label: "Export formats: Markdown, text, JSON Lines and JSON"
      - value: "{version}"
        label: "Current release · {license}"

  - type: section
    title: Recalls, renames and cleanups lose nothing
    index: 1/3
    lede: A message keeps the form it first entered the vault in. If WeChat later shows it recalled or changed, the new form is stored as a revision, and the export still shows the original, marked as recalled. Renamed contacts and groups keep their history, and old databases WeChat cleans up stay in the vault.

  - type: section
    title: Organise once, the exports follow
    index: 2/3
    lede: "`chatvault chat set 家人群 --folder family --alias Family` names a chat and files it in a folder; `--hide` keeps official accounts and the like out. On the next sync the exported files move with it and the old ones are removed. A second export, say group chats only in another folder, is one `chatvault export add`."

  - type: section
    title: Read your history in the terminal
    index: 3/3
    lede: "Run `chatvault` on its own for the interactive screen: every chat by recent activity, `/` to filter, enter to read month by month, `a` for an alias, `f` for a folder, `h` to hide, `s` to sync in the background. For scripts, `sync`, `chats`, `show` and `export` each work on their own."
---
