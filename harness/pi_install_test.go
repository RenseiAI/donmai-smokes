package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePiBinary_NewerPATHDoesNotBypassExactPin(t *testing.T) {
	tests := []struct {
		name    string
		pinEnv  string
		wantPin string
	}{
		{name: "default pin", wantPin: PiPinnedVersion},
		{name: "explicit pin", pinEnv: "0.80.11", wantPin: "0.80.11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			pathPi := writeFakePi(t, binDir, "0.84.2")
			npmPath := writeFakePiNPM(t, binDir)
			setPiResolverTestEnv(t, binDir, tt.pinEnv)

			got := EnsurePiBinary(t)
			if got == pathPi {
				t.Fatalf("EnsurePiBinary reused newer PATH pi %q; want an isolated exact-pin install", got)
			}
			if version, ok := probePiVersion(got); !ok || version != tt.wantPin {
				t.Fatalf("resolved pi version = %q (ok=%t), want exact pin %q", version, ok, tt.wantPin)
			}
			installLog, err := os.ReadFile(npmPath + ".log")
			if err != nil {
				t.Fatalf("read fake npm install log: %v", err)
			}
			wantPackage := piNpmPackage + "@" + tt.wantPin
			if strings.TrimSpace(string(installLog)) != wantPackage {
				t.Fatalf("npm installed %q, want %q", strings.TrimSpace(string(installLog)), wantPackage)
			}
		})
	}
}

func TestEnsurePiBinary_ExactPATHVersionReused(t *testing.T) {
	tests := []struct {
		name   string
		pinEnv string
	}{
		{name: "default pin"},
		{name: "explicit pin", pinEnv: "0.80.11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantPin := tt.pinEnv
			if wantPin == "" {
				wantPin = PiPinnedVersion
			}
			binDir := t.TempDir()
			pathPi := writeFakePi(t, binDir, wantPin)
			npmPath := writeFakePiNPM(t, binDir)
			setPiResolverTestEnv(t, binDir, tt.pinEnv)

			if got := EnsurePiBinary(t); got != pathPi {
				t.Fatalf("EnsurePiBinary = %q, want exact PATH binary %q", got, pathPi)
			}
			if _, err := os.Stat(npmPath + ".log"); !os.IsNotExist(err) {
				t.Fatalf("npm was invoked despite exact PATH match: stat error=%v", err)
			}
		})
	}
}

func TestEnsurePiBinary_ExplicitBinaryOverrideIsVerbatim(t *testing.T) {
	override := filepath.Join(t.TempDir(), "operator-pi")
	t.Setenv(EnvPiBin, override)
	t.Setenv(EnvPiPin, "0.80.11")
	t.Setenv("PATH", t.TempDir())

	if got := EnsurePiBinary(t); got != override {
		t.Fatalf("EnsurePiBinary = %q, want explicit override %q", got, override)
	}
}

func setPiResolverTestEnv(t *testing.T, binDir, pin string) {
	t.Helper()
	t.Setenv(EnvPiBin, "")
	t.Setenv(EnvPiPin, pin)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeFakePi(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "pi")
	script := "#!/bin/sh\n" +
		"if [ \"${1-}\" = \"--version\" ]; then\n" +
		"  printf '%s\\n' 'pi " + version + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return path
}

func writeFakePiNPM(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "npm")
	const script = `#!/bin/sh
prefix=
package=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --prefix)
      shift
      prefix=$1
      ;;
    @earendil-works/pi-coding-agent@*)
      package=$1
      ;;
  esac
  shift
done
if [ -z "$prefix" ] || [ -z "$package" ]; then
  exit 64
fi
pin=${package##*@}
mkdir -p "$prefix/bin"
printf '%s\n' \
  '#!/bin/sh' \
  'if [ "${1-}" = "--version" ]; then' \
  "  printf '%s\\n' 'pi $pin'" \
  '  exit 0' \
  'fi' \
  'exit 1' > "$prefix/bin/pi"
chmod 700 "$prefix/bin/pi"
printf '%s\n' "$package" > "$0.log"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake npm: %v", err)
	}
	return path
}
