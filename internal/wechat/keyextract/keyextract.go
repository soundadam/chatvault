// Package keyextract reads WeChat's database keys from the running process.
//
// WeChat 4 keeps each database's SQLCipher key only in the running process's
// memory, so chatvault cannot read it from disk. This package runs a vendored
// helper that observes the encryption calls the databases make. chatvault then
// verifies the keys against the databases and imports the ones that open them.
//
// The helper lives in this repository together with its Apache-2.0 license
// (see UPSTREAM.md), so `chatvault keys extract` does not need the upstream
// repository to be cloned, or even to still exist, at run time. The trade-off
// is a Python dependency: the helper's dependencies are installed into a
// private virtual environment on first use.
package keyextract

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed extract_key.py
var script []byte

const (
	// Upstream is where the extractor comes from.
	Upstream = "https://github.com/3351666087/wechat-key-macos"
	// UpstreamCommit is the commit extract_key.py was copied from.
	UpstreamCommit = "6594ece61c37bf60aa8fdbb75a5ec2b9cebc8cbb"
	// UpstreamSHA256 is the script's hash, so a changed copy is noticed.
	UpstreamSHA256 = "7d2809b0904937c56a1f88cae5199de7acba10ac742ba00df7054300823c9dc9"

	// FridaVersion is pinned: frida 17 removed the JavaScript bridges the
	// extractor relies on.
	FridaVersion = "16.7.19"
	// cryptoModule is all the extractor needs besides frida.
	cryptoModule = "pycryptodome>=3.19"
)

// ErrNoWeChat means the WeChat process is not running.
var ErrNoWeChat = errors.New("WeChat is not running")

// ToolsDir is where the extractor's private Python environment lives.
func ToolsDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "chatvault", "tools"), nil
}

// Python finds a Python 3.8 or newer to build the extractor's environment.
// frida's wheels are abi3 and work on any of them.
func Python(ctx context.Context) (string, error) {
	var paths []string
	if p, err := exec.LookPath("python3"); err == nil {
		paths = append(paths, p)
	}
	paths = append(paths, "/opt/homebrew/bin/python3", "/usr/local/bin/python3", "/usr/bin/python3")
	var tried []string
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		v, err := pythonVersion(ctx, p)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if v.atLeast(3, 8) {
			return p, nil
		}
		tried = append(tried, fmt.Sprintf("%s: Python %s", p, v))
	}
	return "", fmt.Errorf("no Python 3.8 or newer found (%s); install one with `brew install python`", strings.Join(tried, "; "))
}

type pyVersion struct{ major, minor int }

func (v pyVersion) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }
func (v pyVersion) atLeast(major, minor int) bool {
	return v.major > major || v.major == major && v.minor >= minor
}

// pythonVersion asks an interpreter for its version rather than trusting its
// name: /usr/bin/python3 may be the Xcode stub that only opens an installer.
func pythonVersion(ctx context.Context, path string) (pyVersion, error) {
	out, err := exec.CommandContext(ctx, path, "-c", "import sys; print(sys.version_info[0], sys.version_info[1])").Output()
	if err != nil {
		return pyVersion{}, err
	}
	var v pyVersion
	if _, err := fmt.Sscanf(string(out), "%d %d", &v.major, &v.minor); err != nil || v.major < 3 {
		return pyVersion{}, fmt.Errorf("unexpected version output %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// Env is the private Python environment the extractor runs in.
type Env struct {
	Python string // interpreter with frida and pycryptodome
	Script string // installed copy of the vendored extractor
}

// Prepare creates the private environment under ToolsDir, installs frida and
// pycryptodome on first use, and writes the vendored extractor there. It is
// idempotent, and logf, if not nil, receives progress lines.
func Prepare(ctx context.Context, python string, logf func(string, ...any)) (*Env, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dir, err := ToolsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	venv := filepath.Join(dir, "venv")
	vpy := filepath.Join(venv, "bin", "python")
	if _, err := os.Stat(vpy); errors.Is(err, os.ErrNotExist) {
		logf("creating a private Python environment in %s", venv)
		if out, err := exec.CommandContext(ctx, python, "-m", "venv", venv).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("python -m venv %s: %w%s", venv, err, tail(out))
		}
	} else if err != nil {
		return nil, err
	}
	if err := install(ctx, vpy, logf); err != nil {
		return nil, err
	}
	// Write through a temporary file: a copy left behind by an older run (or
	// by chatvault itself run as root) must not block the update.
	scriptPath := filepath.Join(dir, "extract_key.py")
	tmp, err := os.CreateTemp(dir, "extract_key-*.py")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(script); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), scriptPath); err != nil {
		return nil, err
	}
	return &Env{Python: vpy, Script: scriptPath}, nil
}

// depsCheck succeeds when the environment holds a usable frida 16 and
// pycryptodome.
const depsCheck = `import frida, Crypto; assert frida.__version__.split(".")[0] == "16"`

func install(ctx context.Context, vpy string, logf func(string, ...any)) error {
	if exec.CommandContext(ctx, vpy, "-c", depsCheck).Run() == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	logf("installing the helper's dependencies (the first run downloads about 40 MB)")
	args := []string{"-m", "pip", "install", "--disable-pip-version-check", "--no-input", "-q",
		"frida==" + FridaVersion, cryptoModule}
	if out, err := exec.CommandContext(ctx, vpy, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("installing the helper's dependencies into %s: %w%s\nremove that directory and retry if it stays broken", filepath.Dir(vpy), err, tail(out))
	}
	if err := exec.CommandContext(ctx, vpy, "-c", depsCheck).Run(); err != nil {
		return fmt.Errorf("%s has no usable helper environment after installing it (%v); remove that directory and retry", filepath.Dir(vpy), err)
	}
	return nil
}

// tail adds command output to an error, bounded so a wall of pip output does
// not bury the actual error.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	const max = 1500
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return "\n" + s
}

// PID returns the running WeChat process id.
func PID() (int, error) {
	for _, name := range []string{"WeChat", "Weixin"} {
		out, err := exec.Command("pgrep", "-x", name).Output()
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(out)) {
			if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
				return pid, nil
			}
		}
	}
	return 0, ErrNoWeChat
}

// App finds the installed WeChat application bundle.
func App() (string, error) {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/Applications/WeChat.app",
		filepath.Join(home, "Applications/WeChat.app"),
		"/Applications/Weixin.app",
	} {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("WeChat.app not found in /Applications; install WeChat for Mac 4")
}

// Debuggable reports whether the app is signed so that Frida may attach to it,
// which is what the get-task-allow entitlement permits.
func Debuggable(ctx context.Context, app string) (bool, error) {
	out, err := exec.CommandContext(ctx, "codesign", "-d", "--entitlements", ":-", app).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("codesign -d %s: %w%s", app, err, tail(out))
	}
	return hasGetTaskAllow(out), nil
}

func hasGetTaskAllow(signed []byte) bool {
	return bytes.Contains(signed, []byte("get-task-allow"))
}

// entitlements carries only get-task-allow: an ad-hoc signature cannot keep
// WeChat's restricted entitlements (app groups, application identifier), so
// they are dropped the way wechat-key-macos drops them.
const entitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>com.apple.security.get-task-allow</key><true/>
</dict></plist>
`

// Resign re-signs the app ad-hoc with get-task-allow so Frida can attach. It
// asks for the password with sudo, needs a terminal, and changes the app:
// WeChat has to be quit and reopened afterwards, and reinstalling WeChat
// restores Apple's signature.
func Resign(ctx context.Context, app string, stdin io.Reader, stdout, stderr io.Writer) error {
	f, err := os.CreateTemp("", "chatvault-entitlements-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(entitlements); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("sudo not found")
	}
	cmd := exec.CommandContext(ctx, sudo, "codesign", "--force", "--sign", "-", "--entitlements", f.Name(), app)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("re-signing %s: %w", app, err)
	}
	return nil
}

// Capture runs the extractor under sudo for wait seconds, writing the keys it
// finds to outFile. While it runs, the user has to click around WeChat so that
// WeChat reads more databases and the extractor sees more keys. The
// extractor's own output is redacted on the way to stdout: it prints the keys
// it finds, and chatvault does not print keys.
func Capture(ctx context.Context, env *Env, dbDir, outFile string, wait int, stdin io.Reader, stdout, stderr io.Writer) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("sudo not found")
	}
	redact := NewRedactor(stdout)
	defer redact.Flush()
	cmd := exec.CommandContext(ctx, sudo, "--", env.Python, env.Script,
		"--db-dir", dbDir, "--output", outFile, "--wait", strconv.Itoa(wait))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, redact, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("the extractor failed (%v); its messages above say why", err)
	}
	return nil
}

// keyHex matches the 64 hex characters of a 32-byte key.
var keyHex = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)

// Redactor replaces key-sized hex runs with <key>, line by line so that partial
// writes cannot hide a key across a chunk boundary. Callers must Flush.
type Redactor struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

func NewRedactor(w io.Writer) *Redactor { return &Redactor{w: w} }

func (r *Redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		line := r.buf[:i+1]
		r.buf = r.buf[i+1:]
		if _, err := r.w.Write(keyHex.ReplaceAll(line, []byte("<key>"))); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes a trailing line without a newline.
func (r *Redactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return nil
	}
	line := r.buf
	r.buf = nil
	_, err := r.w.Write(keyHex.ReplaceAll(line, []byte("<key>")))
	return err
}
