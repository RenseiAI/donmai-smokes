package harness

// control_token.go — the daemon control-token seam for smokes that drive a
// live daemon's mutating /api/daemon/* routes.
//
// A daemon that ships the control-API gate mints a per-install bearer token
// into its state home at startup and refuses every non-GET control request
// that does not carry `Authorization: Bearer <token>` (401), or refuses them
// all (503) when it could not mint one. Read-only GET routes stay open.
//
// The harness mirrors that contract from the outside: it resolves where the
// spawned daemon keeps its token (from the exact env the daemon was started
// with), reads the file lazily at request time, and attaches the header on
// mutating requests only. A daemon that predates the gate never writes the
// file, so the harness sends no header and behaves exactly as before.
//
// The token value is never logged, never put in an error, and never exported
// to a subprocess: CLI subprocesses receive only the file PATH
// (ControlTokenFileEnv) and read the credential themselves.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ControlTokenFileEnv is the variable donmai reads to override the
	// control-token file path. An absolute value wins over the state-home
	// default; a relative value resolves to no path at all (the daemon then
	// fails closed). CLI subprocesses aimed at a harness daemon get this
	// variable so they authenticate even when their HOME differs from the
	// daemon's.
	ControlTokenFileEnv = "DONMAI_CONTROL_TOKEN_FILE"

	// stateHomeEnv anchors donmai's state tree (<base>/.donmai/) in place of
	// the user's home directory.
	stateHomeEnv = "DONMAI_STATE_HOME"

	// controlTokenStateDir and controlTokenFileName locate the token under
	// the state home: <base>/.donmai/control-token.
	controlTokenStateDir = ".donmai"
	controlTokenFileName = "control-token"
)

// ControlTokenFileFromEnv resolves the control-token file a donmai daemon
// started with env will use, mirroring donmai's own resolution:
//
//  1. DONMAI_CONTROL_TOKEN_FILE when set: its value if absolute, otherwise
//     "" (donmai treats a relative override as unresolved).
//  2. <DONMAI_STATE_HOME>/.donmai/control-token when the state home is set.
//  3. <HOME>/.donmai/control-token.
//
// When a key repeats, the last assignment wins, matching how os/exec
// de-duplicates a child environment. Returns "" when nothing resolves; the
// harness then sends no credential rather than guessing a shared path.
func ControlTokenFileFromEnv(env []string) string {
	if override := strings.TrimSpace(lastEnvValue(env, ControlTokenFileEnv)); override != "" {
		if filepath.IsAbs(override) {
			return override
		}
		return ""
	}
	base := lastEnvValue(env, stateHomeEnv)
	if base == "" {
		base = lastEnvValue(env, "HOME")
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, controlTokenStateDir, controlTokenFileName)
}

// LoadControlToken reads and trims the control token at path. It returns
// ("", nil) for an empty path or a missing file — the shape of a daemon that
// predates the control token — so callers can fall back to sending no
// credential. Any other read failure is an error that names the path, never
// the contents.
func LoadControlToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path resolved from the harness-owned daemon env
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read daemon control token %q: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// AttachControlToken sets `Authorization: Bearer <token>` on req when the
// method mutates and tokenFile holds a token. It mirrors the daemon gate,
// which lets only GET through untouched: GET (and the zero-value method,
// which net/http sends as GET) never carries the credential, and every
// other method does. With no token file it leaves req untouched.
func AttachControlToken(req *http.Request, tokenFile string) error {
	if req == nil {
		return errors.New("attach daemon control token: nil request")
	}
	if !controlTokenRequired(req.Method) {
		return nil
	}
	token, err := LoadControlToken(tokenFile)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// controlTokenRequired reports whether the daemon gate demands the token
// for method.
func controlTokenRequired(method string) bool {
	return method != "" && method != http.MethodGet
}

// lastEnvValue returns the value of the last KEY=VALUE entry for key.
func lastEnvValue(env []string, key string) string {
	value := ""
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = kv[len(prefix):]
		}
	}
	return value
}
