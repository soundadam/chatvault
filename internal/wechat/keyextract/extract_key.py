#!/usr/bin/env python3
"""
extract_key.py — Extract WeChat 4.x (macOS) SQLCipher database keys via Frida.

Self-contained: only needs `frida` (<17) and `pycryptodome`.
No dependency on wechat-cli.

WHY THIS EXISTS
---------------
WeChat 4.x on macOS encrypts its SQLite databases with SQLCipher (AES-256-CBC).
Each database has its own per-file key:

    enc_key_i = PBKDF2-HMAC-SHA512(password, salt_i, 256000, 32)

where `password` (the account master password) is the same for all databases and
`salt_i` is the first 16 bytes of each database file.

Older tools scanned process memory for a literal `x'<hexkey><hexsalt>'` PRAGMA
string. WeChat >= ~4.1.x no longer keeps that string resident, and the derived
keys are not reliably found as aligned raw bytes, so memory scanning fails.

This tool instead hooks the macOS CommonCrypto AES functions
(CCCrypt / CCCryptorCreate / CCCryptorCreateWithMode) that SQLCipher calls to
decrypt *every database page*. WeChat continuously reads its databases while
running, so these fire constantly. We capture the 32-byte AES keys passed in and
verify each against every database's page-1 HMAC to learn which key belongs to
which database. (We also hook CCKeyDerivationPBKDF; if a derivation happens we
can capture the password and derive keys for all databases at once.)

VERIFIED ENVIRONMENT (2026-09)
------------------------------
- WeChat for Mac 4.1.13 (also expected to work across 4.0–4.1.x)
- macOS 26.6.2, Apple Silicon (arm64)
- Python 3.14 (works on 3.10+)
- frida == 16.7.19   (MUST be < 17: frida 17 removed the built-in bridges/behavior relied on here)
- pycryptodome (any recent 3.x)

REQUIREMENTS
------------
- Run as root (sudo): reading another process's memory / attaching needs it.
- WeChat must be running and logged in.
- WeChat must be debuggable: ad-hoc re-sign it once with get-task-allow
  (see README "task_for_pid" section).
- Terminal needs Full Disk Access to read the WeChat container.

USAGE
-----
    sudo python3 extract_key.py                 # auto-detect db dir, write keys.json
    sudo python3 extract_key.py --db-dir <path> # explicit db_storage dir
    sudo python3 extract_key.py --decrypt out/  # also decrypt matched DBs into out/
    sudo python3 extract_key.py --wait 240      # capture window seconds (default 240)

While it runs, click around WeChat (open chats, Contacts, Favorites, Moments,
stickers, search) so more databases get read and more keys are captured.
"""

import argparse
import glob
import hashlib
import hmac as hmac_mod
import json
import os
import struct
import sys
import time

# ---------------------------------------------------------------------------
# SQLCipher parameters for WeChat 4.x (see reference/method.md)
# ---------------------------------------------------------------------------
PAGE_SZ = 4096
KEY_SZ = 32
SALT_SZ = 16
RESERVE_SZ = 80          # IV(16) + HMAC-SHA512(64)
KDF_ITER = 256000        # PBKDF2-HMAC-SHA512 rounds
FAST_KDF_ITER = 2        # rounds to derive the HMAC key
SQLITE_HDR = b"SQLite format 3\x00"

_PRF_HASH = {1: "sha1", 2: "sha224", 3: "sha256", 4: "sha384", 5: "sha512"}


def verify_enc_key(enc_key: bytes, page1: bytes) -> bool:
    """Verify a 32-byte AES key against a database's first page via HMAC-SHA512."""
    salt = page1[:SALT_SZ]
    mac_salt = bytes(b ^ 0x3A for b in salt)
    mac_key = hashlib.pbkdf2_hmac("sha512", enc_key, mac_salt, FAST_KDF_ITER, dklen=KEY_SZ)
    hmac_data = page1[SALT_SZ: PAGE_SZ - RESERVE_SZ + 16]      # data + IV
    stored_hmac = page1[PAGE_SZ - 64: PAGE_SZ]
    hm = hmac_mod.new(mac_key, hmac_data, hashlib.sha512)
    hm.update(struct.pack("<I", 1))
    return hm.digest() == stored_hmac


def decrypt_page(enc_key: bytes, page: bytes, pgno: int) -> bytes:
    from Crypto.Cipher import AES
    iv = page[PAGE_SZ - RESERVE_SZ: PAGE_SZ - RESERVE_SZ + 16]
    if pgno == 1:
        ct = page[SALT_SZ: PAGE_SZ - RESERVE_SZ]
        dec = AES.new(enc_key, AES.MODE_CBC, iv).decrypt(ct)
        return SQLITE_HDR + dec + b"\x00" * RESERVE_SZ
    ct = page[:PAGE_SZ - RESERVE_SZ]
    dec = AES.new(enc_key, AES.MODE_CBC, iv).decrypt(ct)
    return dec + b"\x00" * RESERVE_SZ


def full_decrypt(db_path: str, out_path: str, enc_key: bytes) -> int:
    size = os.path.getsize(db_path)
    total = size // PAGE_SZ
    os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
    with open(db_path, "rb") as fin, open(out_path, "wb") as fout:
        for pgno in range(1, total + 1):
            page = fin.read(PAGE_SZ)
            if len(page) < PAGE_SZ:
                break
            fout.write(decrypt_page(enc_key, page, pgno))
    return total


def collect_db_files(db_dir: str):
    """Return [(rel, abs_path, size, salt_hex, page1), ...] for encrypted .db files."""
    out = []
    for root, _dirs, files in os.walk(db_dir):
        for name in files:
            if not name.endswith(".db") or name.endswith(("-wal", "-shm")):
                continue
            path = os.path.join(root, name)
            try:
                size = os.path.getsize(path)
                if size < PAGE_SZ:
                    continue
                with open(path, "rb") as f:
                    page1 = f.read(PAGE_SZ)
            except OSError:
                continue
            if page1[:16] == SQLITE_HDR:      # already-plaintext db, skip
                continue
            rel = os.path.relpath(path, db_dir)
            out.append((rel, path, size, page1[:SALT_SZ].hex(), page1))
    return out


def auto_detect_db_dir():
    base = os.path.expanduser(
        "~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files")
    cands = [m for m in glob.glob(os.path.join(base, "*", "db_storage")) if os.path.isdir(m)]
    if not cands:
        return None
    # newest by message/ mtime
    def mt(p):
        d = os.path.join(p, "message")
        try:
            return os.path.getmtime(d if os.path.isdir(d) else p)
        except OSError:
            return 0
    cands.sort(key=mt, reverse=True)
    return cands[0]


def find_wechat_pid():
    import subprocess
    for name in ("WeChat", "Weixin"):
        try:
            out = subprocess.run(["pgrep", "-x", name], capture_output=True,
                                 text=True, timeout=10).stdout
        except Exception:
            out = ""
        for line in out.split():
            if line.strip().isdigit():
                return int(line.strip())
    return None


FRIDA_JS = r"""
function getFn(n){ try{return Module.getGlobalExportByName(n);}catch(e){try{return Module.findExportByName(null,n);}catch(e2){return null;}} }
let sent = {};
function pushKey(ptr, src){
    try{
        let arr = Array.from(new Uint8Array(ptr.readByteArray(32)));
        let hex = arr.map(function(x){return ('0'+(x&0xff).toString(16)).slice(-2);}).join('');
        if (hex === '0000000000000000000000000000000000000000000000000000000000000000') return;
        if (!sent[hex]) { sent[hex]=1; send({type:'cckey', b:arr, src:src}); }
    }catch(e){}
}
(function(){ let f=getFn('CCCrypt'); if(!f)return;
  Interceptor.attach(f,{onEnter:function(a){try{ if(a[1].toInt32()===0 && a[4].toInt32()===32) pushKey(a[3],'CCCrypt'); }catch(e){}}});
  send({type:'hooked',name:'CCCrypt'}); })();
(function(){ let f=getFn('CCCryptorCreate'); if(!f)return;
  Interceptor.attach(f,{onEnter:function(a){try{ if(a[1].toInt32()===0 && a[4].toInt32()===32) pushKey(a[3],'CCCryptorCreate'); }catch(e){}}});
  send({type:'hooked',name:'CCCryptorCreate'}); })();
(function(){ let f=getFn('CCCryptorCreateWithMode'); if(!f)return;
  Interceptor.attach(f,{onEnter:function(a){try{ if(a[2].toInt32()===0 && a[6].toInt32()===32) pushKey(a[5],'CCCryptorCreateWithMode'); }catch(e){}}});
  send({type:'hooked',name:'CCCryptorCreateWithMode'}); })();
(function(){ let f=getFn('CCKeyDerivationPBKDF'); if(!f)return;
  Interceptor.attach(f,{
    onEnter:function(a){try{
      let alg=a[0].toInt32(), pl=a[2].toInt32(), sl=a[4].toInt32(), dl=a[8].toInt32();
      if(alg===2 && dl===32 && a[6].toInt32()>=10000 && pl>0 && pl<=128 && sl>0 && sl<=64){
        this.cap={pw:Array.from(new Uint8Array(a[1].readByteArray(pl))),
                  salt:Array.from(new Uint8Array(a[3].readByteArray(sl))),
                  prf:a[5].toInt32(), rounds:a[6].toInt32(), dkLen:dl, dkPtr:a[7]};
      }
    }catch(e){this.cap=null;}},
    onLeave:function(r){ if(this.cap){ try{this.cap.dk=Array.from(new Uint8Array(this.cap.dkPtr.readByteArray(this.cap.dkLen)));}catch(e){this.cap.dk=null;}
      delete this.cap.dkPtr; this.cap.type='kdf'; send(this.cap);} }
  });
  send({type:'hooked',name:'CCKeyDerivationPBKDF'}); })();
"""


def extract(db_dir, output, wait, decrypt_dir):
    try:
        import frida
    except ImportError:
        sys.exit("ERROR: frida not installed. Run: pip install 'frida<17' pycryptodome")

    if os.geteuid() != 0:
        sys.exit("ERROR: must run as root (sudo).")

    dbs = collect_db_files(db_dir)
    if not dbs:
        sys.exit(f"ERROR: no encrypted .db files under {db_dir}")
    salt_to_dbs = {}
    for rel, _p, _s, salt, _pg in dbs:
        salt_to_dbs.setdefault(salt, []).append(rel)

    pid = find_wechat_pid()
    if not pid:
        sys.exit("ERROR: WeChat/Weixin process not found (is it running & logged in?)")

    print("=" * 60)
    print("  WeChat 4.x macOS key extraction (Frida / CommonCrypto)")
    print("=" * 60)
    print(f"[+] databases: {len(dbs)}   distinct salts: {len(salt_to_dbs)}")
    print(f"[+] WeChat PID: {pid}")

    cckeys, seen = [], set()
    captures = []
    stats = {"cckey": 0, "hooked": []}

    def on_message(msg, data):
        if msg.get("type") == "send":
            p = msg.get("payload", {})
            t = p.get("type")
            if t == "hooked":
                stats["hooked"].append(p["name"]); print(f"[+] hooked {p['name']}")
            elif t == "cckey":
                h = bytes(p["b"]).hex()
                if h not in seen:
                    seen.add(h); cckeys.append(h); stats["cckey"] += 1
            elif t == "kdf":
                captures.append(p)
        elif msg.get("type") == "error":
            print(f"[!] script error: {msg.get('description')}")

    device = frida.get_local_device()
    try:
        session = device.attach(pid)
    except Exception as e:
        sys.exit(f"ERROR: frida attach failed: {e}\n"
                 "Ensure sudo + WeChat re-signed with get-task-allow (see README).")
    script = session.create_script(FRIDA_JS)
    script.on("message", on_message)
    script.load()

    print(f"\n[*] capturing for up to {wait}s. Click around WeChat to read more DBs.\n")

    key_map = {}
    remaining = {salt: pg for _r, _p, _s, salt, pg in dbs}
    tested = set()
    t0 = time.time()
    last = 0

    def match():
        for h in list(cckeys):
            if h in tested:
                continue
            tested.add(h)
            k = bytes.fromhex(h)
            for salt in list(remaining):
                if verify_enc_key(k, remaining[salt]):
                    key_map[salt] = h
                    remaining.pop(salt)
                    print(f"  [FOUND] {', '.join(salt_to_dbs[salt])}  enc_key={h}")
                    break

    def try_password():
        for c in captures:
            if c.get("dk") is None:
                continue
            pw, salt, dk = bytes(c["pw"]), bytes(c["salt"]), bytes(c["dk"])
            if len(pw) != 32 or len(salt) != 16:
                continue
            hn = _PRF_HASH.get(c["prf"], "sha512")
            if hashlib.pbkdf2_hmac(hn, pw, salt, c["rounds"], 32) == dk:
                return pw, c["rounds"], hn
        return None

    pw = None
    while time.time() - t0 < wait and remaining:
        match()
        if pw is None:
            got = try_password()
            if got:
                pw, rounds, prf = got
                print(f"\n[+] captured PASSWORD (prf={prf} rounds={rounds}); deriving all DBs...")
                for salt in list(remaining):
                    dk = hashlib.pbkdf2_hmac(prf, pw, bytes.fromhex(salt), rounds, 32)
                    if verify_enc_key(dk, remaining[salt]):
                        key_map[salt] = dk.hex(); remaining.pop(salt)
        now = time.time()
        if now - last >= 5:
            last = now
            print(f"  ... {stats['cckey']} AES keys seen, "
                  f"{len(key_map)}/{len(salt_to_dbs)} DBs matched, {int(now - t0)}s")
        time.sleep(0.5)
    match()

    try:
        script.unload(); session.detach()
    except Exception:
        pass

    if not key_map:
        sys.exit("ERROR: no keys matched. "
                 f"hooked={stats['hooked']} aes_keys_seen={stats['cckey']} kdf={len(captures)}. "
                 "See README troubleshooting.")

    result = {}
    for rel, _p, sz, salt, _pg in dbs:
        if salt in key_map:
            result[rel] = {"enc_key": key_map[salt], "salt": salt,
                           "size_mb": round(sz / 1048576, 1)}
    with open(output, "w", encoding="utf-8") as f:
        json.dump(result, f, indent=2, ensure_ascii=False)

    missing = [rel for rel, _p, _s, salt, _pg in dbs if salt not in key_map]
    print(f"\n[+] {len(result)}/{len(dbs)} databases keyed -> {output}")
    if missing:
        print("[!] not covered (not read during capture; re-run after opening those "
              "features in WeChat):")
        for rel in missing:
            print(f"    {rel}")

    if decrypt_dir:
        print(f"\n[*] decrypting matched databases into {decrypt_dir} ...")
        for rel, path, _sz, salt, _pg in dbs:
            if salt in key_map:
                out = os.path.join(decrypt_dir, rel)
                full_decrypt(path, out, bytes.fromhex(key_map[salt]))
                print(f"    {rel} -> {out}")
    return result


def main():
    ap = argparse.ArgumentParser(description="Extract WeChat 4.x macOS SQLCipher keys via Frida.")
    ap.add_argument("--db-dir", help="WeChat db_storage dir (default: auto-detect)")
    ap.add_argument("--output", default="keys.json", help="output JSON path (default: keys.json)")
    ap.add_argument("--wait", type=int, default=240, help="capture window seconds (default 240)")
    ap.add_argument("--decrypt", metavar="DIR", help="also decrypt matched DBs into DIR")
    args = ap.parse_args()

    db_dir = args.db_dir or auto_detect_db_dir()
    if not db_dir or not os.path.isdir(db_dir):
        sys.exit("ERROR: could not locate db_storage; pass --db-dir")
    print(f"[+] db_dir: {db_dir}")
    extract(db_dir, args.output, args.wait, args.decrypt)


if __name__ == "__main__":
    main()
