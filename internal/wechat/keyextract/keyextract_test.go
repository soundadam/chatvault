package keyextract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs shell scripts")
	}
	dir := t.TempDir()
	key := strings.Repeat("ab", 32)
	// A stand-in for sudo that drops the "--" and runs the extractor.
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n[ \"$1\" = \"--\" ] && shift\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the extractor: writes a keys file and prints the key,
	// as the real one does.
	python := filepath.Join(dir, "python")
	fake := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in --output) out=\"$2\"; shift 2 ;; *) shift ;; esac\n" +
		"done\n" +
		"printf '{\"keys\":[\"" + key + "\"]}' > \"$out\"\n" +
		"echo \"[FOUND] message/message_0.db enc_key=" + key + "\"\n"
	if err := os.WriteFile(python, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	outFile := filepath.Join(dir, "keys.json")
	var stdout, stderr bytes.Buffer
	if err := Capture(context.Background(), &Env{Python: python, Script: "extract_key.py"}, "/db", outFile, 30, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), key) {
		t.Fatalf("capture printed the key: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "enc_key=<key>") {
		t.Fatalf("capture output = %q, want the key redacted", stdout.String())
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("the keys file was not written: %v", err)
	}
}

func TestVendoredScriptIsTheUpstreamCopy(t *testing.T) {
	sum := sha256.Sum256(script)
	if got := hex.EncodeToString(sum[:]); got != UpstreamSHA256 {
		t.Fatalf("extract_key.py hashes to %s, want %s; update UpstreamSHA256, UPSTREAM.md and the commit if the vendored copy changed", got, UpstreamSHA256)
	}
	upstream, err := os.ReadFile("UPSTREAM.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{UpstreamCommit, UpstreamSHA256, Upstream} {
		if !strings.Contains(string(upstream), want) {
			t.Errorf("UPSTREAM.md does not mention %s", want)
		}
	}
}

func TestRedactor(t *testing.T) {
	key := strings.Repeat("ab", 32)
	salt := strings.Repeat("cd", 16)
	line := "[FOUND] message/message_0.db enc_key=" + key + " salt=" + salt + "\n"
	cut := strings.Index(line, key) + 20 // inside the key
	var out bytes.Buffer
	r := NewRedactor(&out)
	if _, err := r.Write([]byte(line[:cut])); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote a partial line: %q", out.String())
	}
	if _, err := r.Write([]byte(line[cut:])); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, key) {
		t.Fatalf("key leaked: %q", got)
	}
	if !strings.Contains(got, "enc_key=<key>") {
		t.Fatalf("key not replaced: %q", got)
	}
	if !strings.Contains(got, "salt="+salt) {
		t.Fatalf("salt should stay readable: %q", got)
	}
}

func TestRedactorFlushesWithoutNewline(t *testing.T) {
	key := strings.Repeat("0f", 32)
	var out bytes.Buffer
	r := NewRedactor(&out)
	if _, err := r.Write([]byte("key " + key)); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, key) || !strings.Contains(got, "key <key>") {
		t.Fatalf("flushed %q", got)
	}
}

func TestHasGetTaskAllow(t *testing.T) {
	if !hasGetTaskAllow([]byte(`<key>com.apple.security.get-task-allow</key><true/>`)) {
		t.Error("signed app with get-task-allow not recognized")
	}
	if hasGetTaskAllow([]byte(`<key>com.apple.security.app-sandbox</key><true/>`)) {
		t.Error("app without get-task-allow recognized as debuggable")
	}
}

func TestPythonVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "python3")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '3 14'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v, err := pythonVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "3.14" || !v.atLeast(3, 8) || v.atLeast(3, 15) {
		t.Fatalf("version = %v", v)
	}
}
