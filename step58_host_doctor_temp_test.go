package smokes

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// step58 — `host doctor temp`: the temp janitor's ownership rule, end to end
// through the compiled binary.
//
// donmai names every temp artefact its probes, harness sessions and runner
// create "<prefix>p<owner pid>-<random>", and the janitor removes one only
// when that recorded owner is gone and the artefact sat idle past the grace
// period. This smoke plants, in a scratch folder (never the host's real temp
// folder):
//
//   - a recorded artefact whose owner PID is a reaped child (dead),
//   - a recorded artefact whose owner is a live `sleep` this test owns,
//   - an untagged artefact with a registered prefix (legacy, no record),
//   - a shared tool cache (tsx-<uid>),
//
// then runs `host doctor temp --root <scratch> --fix --grace 1s` and asserts
// that exactly the dead owner's artefact is gone. No daemon is contacted:
// without --include-tool-caches the fix never asks for seat state.
func TestStep58HostDoctorTempJanitorOwnershipRule(t *testing.T) {
	afh.SkipIfShort(t, "builds and drives the donmai binary")
	afh.SkipIfToolMissing(t, "sleep", "the live-owner fixture is a sleep process this test owns")
	afh.SkipIfToolMissing(t, "true", "the dead-owner fixture is a reaped true process")

	srcDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	if _, err := os.Stat(filepath.Join(srcDir, "tempdoctor", "fix.go")); err != nil {
		afh.DeclineLive(t, "donmai checkout at %q predates `host doctor temp` (tempdoctor/fix.go) — point %s at a checkout that has it: %v",
			srcDir, inFlightSourceDirEnv, err)
	}
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: srcDir, OutputPath: filepath.Join(t.TempDir(), "donmai")})

	root := t.TempDir()
	reaped := exec.Command("true")
	if err := reaped.Run(); err != nil {
		t.Fatalf("run dead-owner fixture: %v", err)
	}
	live := exec.Command("sleep", "120")
	if err := live.Start(); err != nil {
		t.Fatalf("start live-owner fixture: %v", err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })

	plant := func(name string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dead := plant("donmai-claude-usage-p" + strconv.Itoa(reaped.Process.Pid) + "-1")
	liveOwned := plant("donmai-signal-guard-p" + strconv.Itoa(live.Process.Pid) + "-2")
	legacy := plant("donmai-claude-login-3920481723")
	toolCache := plant("tsx-501")
	// The janitor measures idleness on change times, which cannot be
	// backdated; wait out the 1s grace instead.
	time.Sleep(1500 * time.Millisecond)

	home := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"host", "doctor", "temp", "--root", root, "--grace", "1s", "--json"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + home, "TMPDIR=" + t.TempDir(), "DONMAI_STATE_HOME=" + home, "NO_COLOR=1"}
		out, err := cmd.Output()
		if ctx.Err() != nil {
			t.Fatalf("doctor temp timed out: %s", out)
		}
		if err != nil {
			t.Fatalf("doctor temp %v: %v\n%s", args, err, out)
		}
		return out
	}

	type verdicts map[string]int
	var report struct {
		Report struct {
			Groups []struct {
				Class    string   `json:"class"`
				Label    string   `json:"label"`
				Verdicts verdicts `json:"verdicts"`
			} `json:"groups"`
		} `json:"report"`
	}
	if err := json.Unmarshal(run(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	want := map[string]verdicts{
		"Claude usage probe": {"reclaimable": 1},
		"signal guard":       {"live": 1},
		"Claude login probe": {"legacy": 1},
	}
	seenToolCache := false
	for _, g := range report.Report.Groups {
		if g.Class == "tool-cache" && g.Label == "tsx cache" {
			seenToolCache = true
		}
		if w, ok := want[g.Label]; ok {
			for k, n := range w {
				if g.Verdicts[k] != n {
					t.Errorf("report group %q verdicts = %v, want %s=%d", g.Label, g.Verdicts, k, n)
				}
			}
			delete(want, g.Label)
		}
	}
	if len(want) != 0 || !seenToolCache {
		t.Errorf("report lacks groups %v (tool cache seen: %v)", want, seenToolCache)
	}

	var fixed struct {
		Fix struct {
			Janitor struct {
				Reclaimed     int `json:"reclaimed"`
				SkippedLive   int `json:"skippedLive"`
				SkippedLegacy int `json:"skippedLegacy"`
			} `json:"janitor"`
		} `json:"fix"`
	}
	if err := json.Unmarshal(run("--fix"), &fixed); err != nil {
		t.Fatalf("decode fix: %v", err)
	}
	if j := fixed.Fix.Janitor; j.Reclaimed != 1 || j.SkippedLive != 1 || j.SkippedLegacy != 1 {
		t.Errorf("fix janitor = %+v, want 1 reclaimed, 1 live, 1 legacy", j)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the dead owner's recorded artefact survived --fix: %v", err)
	}
	for _, keep := range []string{liveOwned, legacy, toolCache} {
		if _, err := os.Stat(filepath.Join(keep, "payload")); err != nil {
			t.Errorf("--fix touched %s: %v", filepath.Base(keep), err)
		}
	}
}
