# Vendored: wechat-key-macos

`extract_key.py` and `LICENSE` are copied verbatim from
<https://github.com/3351666087/wechat-key-macos>, commit
`6594ece61c37bf60aa8fdbb75a5ec2b9cebc8cbb` (2026-09-17), path
`scripts/extract_key.py`, under the Apache License 2.0 (see `LICENSE`).

- sha256 of `extract_key.py`:
  `7d2809b0904937c56a1f88cae5199de7acba10ac742ba00df7054300823c9dc9`
- Local changes: none. The file is embedded in the chatvault binary
  (`//go:embed`), written to `~/Library/Application Support/chatvault/tools/`
  at run time, and run with `sudo` by `chatvault keys extract`.
- Dependencies are installed into a private virtual environment:
  `frida==16.7.19` (frida 17 removed the JS bridges this script uses) and
  `pycryptodome>=3.19`.
- Updating: replace `extract_key.py`, update the commit and hash here and
  `UpstreamCommit` / `UpstreamSHA256` in `keyextract.go`; a test checks that the
  hash, the file and the constant agree.
- The upstream disclaimer applies: use it on your own Mac and your own WeChat
  account only. chatvault only uses the extractor to read keys; it verifies
  them against the databases and never prints them.
