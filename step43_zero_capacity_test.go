package smokes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// The consumer replaces only WorkerCommand with a finite private child. Start,
// YAML decoding/watching, control admission, capacity and reaping are the SUT.
const zeroCapacityConsumer = `package main
import (
 "context"
 "encoding/json"
 "fmt"
 "net"
 "net/http"
 "os"
 "os/signal"
 "path/filepath"
 "strconv"
 "syscall"
 "time"
 "github.com/RenseiAI/donmai/afclient"
 "github.com/RenseiAI/donmai/daemon"
 "gopkg.in/yaml.v3"
)
func member(node *yaml.Node,key string) *yaml.Node {
 if node.Kind==yaml.DocumentNode {node=node.Content[0]}
 for i:=0;i+1<len(node.Content);i+=2{if node.Content[i].Value==key{return node.Content[i+1]}}
 return &yaml.Node{}
}
func worker(root string) error {
 id:=os.Getenv("DONMAI_SESSION_ID")
 if id!="capacity-first"&&id!="capacity-second"{return fmt.Errorf("unexpected fixture session")}
 listener,err:=net.Listen("tcp","127.0.0.1:0");if err!=nil{return err}
 released:=make(chan struct{},1)
 mux:=http.NewServeMux()
 mux.HandleFunc("/ack",func(w http.ResponseWriter,r *http.Request){
  if r.Method!=http.MethodPost{w.WriteHeader(405);return}
  nonce:=r.URL.Query().Get("nonce");if nonce==""{w.WriteHeader(400);return}
  _=json.NewEncoder(w).Encode(map[string]any{"pid":os.Getpid(),"sessionId":id,"nonce":nonce})
 })
 mux.HandleFunc("/release",func(w http.ResponseWriter,r *http.Request){if r.Method!=http.MethodPost{w.WriteHeader(405);return};w.WriteHeader(204);select{case released<-struct{}{}:default:}})
 server:=&http.Server{Handler:mux,ReadHeaderTimeout:time.Second}
 done:=make(chan error,1);go func(){done<-server.Serve(listener)}()
 body,err:=json.Marshal(map[string]any{"pid":os.Getpid(),"sessionId":id,"url":"http://"+listener.Addr().String()});if err!=nil{return err}
 if err:=os.WriteFile(filepath.Join(root,id+".json"),body,0600);err!=nil{return err}
 select{case <-released:case <-time.After(35*time.Second):case err:=<-done:return err}
 ctx,cancel:=context.WithTimeout(context.Background(),time.Second);defer cancel()
 return server.Shutdown(ctx)
}
func run() error {
 if os.Args[1]=="worker"{return worker(os.Args[2])}
 if os.Args[1]=="load"{cfg,err:=daemon.LoadConfig(os.Args[2]);if err!=nil{return err};return json.NewEncoder(os.Stdout).Encode(cfg.Capacity)}
 if os.Args[1]=="yaml-values"{
  raw,err:=os.ReadFile(os.Args[2]);if err!=nil{return err}
  var values map[string]any;if err:=yaml.Unmarshal(raw,&values);err!=nil{return err}
  var node yaml.Node;if err:=yaml.Unmarshal(raw,&node);err!=nil{return err}
  return json.NewEncoder(os.Stdout).Encode(map[string]any{"values":values,"capacityAnchor":member(member(&node,"capacity"),"maxConcurrentSessions").Anchor,"observerAlias":member(&node,"observer").Value,"defaultAnchor":member(&node,"defaultLimit").Anchor})
 }
 port,err:=strconv.Atoi(os.Args[3]);if err!=nil{return err}
 root:=os.Args[4]
 token,err:=afclient.EnsureControlToken(filepath.Join(root,".donmai","control-token"));if err!=nil{return err}
 exe,err:=os.Executable();if err!=nil{return err}
 d:=daemon.New(daemon.Options{ConfigPath:os.Args[2],SkipWizard:true,SkipRegistration:true,HTTPHost:"127.0.0.1",HTTPPort:port,ControlToken:token,RequireControlToken:true,
 SpawnerOptions:daemon.SpawnerOptions{WorkerCommand:[]string{exe,"worker",root},WorktreeParentDir:filepath.Join(root,"worktrees"),BaseEnv:map[string]string{"HOME":root,"PATH":"/usr/bin:/bin","TMPDIR":root}}})
 ctx,cancel:=signal.NotifyContext(context.Background(),syscall.SIGTERM,syscall.SIGINT);defer cancel()
 server:=daemon.NewServer(d)
 serverDone,err:=server.StartBeforeDaemon();if err!=nil{return err}
 if err:=d.Start(ctx);err!=nil{stopCtx,stopCancel:=context.WithTimeout(context.Background(),time.Second);defer stopCancel();_ = server.Shutdown(stopCtx);return err}
 server.DaemonStarted()
 select{case <-ctx.Done():case <-time.After(60*time.Second):case err:=<-serverDone:return err}
 stopCtx,stopCancel:=context.WithTimeout(context.Background(),10*time.Second);defer stopCancel()
 if err:=server.Shutdown(stopCtx);err!=nil{return err}
 return d.Stop(stopCtx)
}
func main(){if err:=run();err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
`

const capacityBaseYAML = `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: smoke-capacity
orchestrator:
  url: http://127.0.0.1:1
projectAdmissionVersion: 2
projectAdmissionMode: enumerated
enabledProjectIds: [capacity-project]
projects: []
`

type capacityStats struct {
	Capacity struct {
		MaxConcurrentSessions int `json:"maxConcurrentSessions"`
	} `json:"capacity"`
	ActiveSessions int `json:"activeSessions"`
}

type capacityHandle struct {
	SessionID  string `json:"sessionId"`
	PID        int    `json:"pid"`
	AcceptedAt string `json:"acceptedAt"`
}

type capacityWorker struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	URL       string `json:"url"`
	Nonce     string `json:"nonce"`
}

func buildCapacityConsumer(t *testing.T, source string) string {
	t.Helper()
	moduleBytes, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
		}
	}
	if version == "" {
		t.Fatal("selected source has no Go directive")
	}
	dir := t.TempDir()
	mod := fmt.Sprintf("module capacity-smoke-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\nreplace github.com/RenseiAI/donmai => %s\n", version, strconv.Quote(source))
	for name, body := range map[string]string{"go.mod": mod, "main.go": zeroCapacityConsumer} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "consumer")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-race", "-o", binary, "."}} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed Go arguments and private consumer module.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("capacity consumer go %v: %v\n%s", args, err, out)
		}
	}
	return binary
}

func capacityRequest(live *afh.LiveDaemon, method, path string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := live.NewRequest(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return raw, resp.StatusCode, err
}

func capacityReadStats(live *afh.LiveDaemon) (capacityStats, error) {
	var stats capacityStats
	raw, code, err := capacityRequest(live, http.MethodGet, "/api/daemon/stats", nil)
	if err != nil || code != http.StatusOK {
		return stats, fmt.Errorf("stats status=%d: %v", code, err)
	}
	return stats, json.Unmarshal(raw, &stats)
}

func capacityWait(live *afh.LiveDaemon, max, active int) error {
	deadline := time.Now().Add(8 * time.Second)
	var stats capacityStats
	var err error
	for time.Now().Before(deadline) {
		stats, err = capacityReadStats(live)
		if err == nil && stats.Capacity.MaxConcurrentSessions == max && stats.ActiveSessions == active {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("capacity=%d active=%d, want %d/%d: %v", stats.Capacity.MaxConcurrentSessions, stats.ActiveSessions, max, active, err)
}

func capacityWorkerCall(worker capacityWorker, route string) (capacityWorker, error) {
	var ack capacityWorker
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, worker.URL+route, nil)
	if err != nil {
		return ack, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return ack, err
	}
	defer func() { _ = resp.Body.Close() }()
	if route == "/release" && resp.StatusCode == http.StatusNoContent {
		return ack, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ack, fmt.Errorf("fixture worker response=%d", resp.StatusCode)
	}
	return ack, json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ack)
}

func capacityStart(t *testing.T, consumer, yaml string) (*afh.LiveDaemon, string, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "donmai-capacity-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".donmai", "daemon.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := afh.PickFreePort()
	if err != nil {
		t.Fatal(err)
	}
	logs := afh.NewLogTail(16 << 10)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	live, err := afh.SpawnDaemon(ctx, afh.SpawnOptions{
		Binary: consumer, Args: []string{"serve", path, strconv.Itoa(port), root}, HomeDir: root,
		Env:            []string{"HOME=" + root, "TMPDIR=" + root, "PATH=/usr/bin:/bin", "DONMAI_STATE_HOME=" + root, "DONMAI_DAEMON_FORCE_STUB=1", "NO_COLOR=1"},
		HealthzBaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), LogSink: logs,
	})
	if err != nil {
		t.Fatalf("public daemon consumer startup: %v\n%s; retained root=%s", err, logs.String(), root)
	}
	t.Cleanup(func() {
		// Release only fixture workers named by our private ready artifacts.
		for _, id := range []string{"capacity-first", "capacity-second"} {
			raw, readErr := os.ReadFile(filepath.Join(root, id+".json"))
			var worker capacityWorker
			if readErr == nil && json.Unmarshal(raw, &worker) == nil {
				_, _ = capacityWorkerCall(worker, "/release")
			}
		}
		stats, statsErr := capacityReadStats(live)
		if statsErr == nil {
			statsErr = capacityWait(live, stats.Capacity.MaxConcurrentSessions, 0)
		}
		live.Stop()
		if statsErr != nil {
			t.Errorf("fixture reaping unproved; retained root=%s: %v\n%s", root, statsErr, logs.String())
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove quiescent fixture root: %v", err)
		}
	})
	t.Logf("public daemon consumer pid=%d private root=%s", live.Cmd.Process.Pid, root)
	return live, path, root
}

func capacityAssertRefused(t *testing.T, live *afh.LiveDaemon, active int) {
	t.Helper()
	raw, code, err := capacityRequest(live, http.MethodPost, "/api/daemon/sessions", []byte(`{"sessionId":"capacity-second","projectId":"capacity-project","requiresRepository":false,"mode":"headless"}`))
	want := fmt.Sprintf("at capacity (%d/0 sessions)", active)
	if err != nil || code != http.StatusBadRequest || !strings.Contains(string(raw), want) {
		t.Fatalf("zero admission status=%d want 400/%q: %v %s", code, want, err, raw)
	}
}

func TestZeroCapacityStartupAndCLI(t *testing.T) {
	afh.SkipIfShort(t, "compiled public daemon capacity and CLI writer smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-daemon smoke")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildCapacityConsumer(t, source)
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: source, Timeout: 3 * time.Minute})
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled CLI and public daemon consumer from "+source)
	for _, fixture := range []struct {
		name, yaml string
		want       int
	}{
		{"omitted", "", 8},
		{"zero", "capacity: {maxConcurrentSessions: 0}\n", 0},
		{"positive", "capacity: {maxConcurrentSessions: 3}\n", 3},
		{"merged-zero", "capacity:\n  <<: {maxConcurrentSessions: 0}\n", 0},
		{"aliased-zero", "fixtureZero: &zero 0\ncapacity: {maxConcurrentSessions: *zero}\n", 0},
		{"null-default", "capacity: {maxConcurrentSessions: null}\n", 8},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			live, _, _ := capacityStart(t, consumer, capacityBaseYAML+fixture.yaml)
			if err := capacityWait(live, fixture.want, 0); err != nil {
				t.Fatal(err)
			}
			if fixture.want == 0 {
				capacityAssertRefused(t, live, 0)
			}
		})
	}
	t.Run("negative-refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "daemon.yaml")
		if err := os.WriteFile(path, []byte(capacityBaseYAML+"capacity: {maxConcurrentSessions: -1}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, consumer, "load", path) //nolint:gosec // private consumer and fixture config.
		cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "must be >= 0") {
			t.Fatalf("negative accepted or wrong refusal: %v %s", err, out)
		}
	})
	for _, fixture := range []struct {
		name, yaml                    string
		checkAnchors                  bool
		wantObserver                  int
		capacityAnchor, defaultAnchor string
	}{
		{name: "cli-positive-to-zero", yaml: "capacity: {maxConcurrentSessions: 3}\n"},
		{name: "cli-omitted-to-zero"},
		{name: "cli-local-scalar-anchor", yaml: "capacity:\n  maxConcurrentSessions: &limit 3\nobserver: *limit\n", checkAnchors: true, wantObserver: 0, capacityAnchor: "limit"},
		{name: "cli-external-scalar-anchor", yaml: "defaultLimit: &limit 3\ncapacity:\n  maxConcurrentSessions: *limit\nobserver: *limit\n", checkAnchors: true, wantObserver: 3, defaultAnchor: "limit"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			live, path, root := capacityStart(t, consumer, capacityBaseYAML+fixture.yaml)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			out, err := afh.RunHermeticAgainstDaemon(ctx, afh.HermeticRunOptions{Binary: binary, Args: []string{"host", "set", "capacity.maxConcurrentSessions", "0", "--config", path, "--host", "127.0.0.1", "--port", strconv.Itoa(live.Port())}, HomeDir: root, DaemonURLEnvVar: "DONMAI_DAEMON_URL", DaemonURL: live.URL, ControlTokenFile: live.ControlTokenFile()})
			if err != nil {
				t.Fatalf("compiled host set: %v %s", err, out)
			}
			if fixture.checkAnchors {
				t.Logf("compiled host set returned success: %s", strings.TrimSpace(out))
			}
			if err := capacityWait(live, 0, 0); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, consumer, "load", path) //nolint:gosec // private consumer reads the actual written config.
			cmd.Env = []string{"HOME=" + root, "PATH=/usr/bin:/bin"}
			var loadErrors bytes.Buffer
			cmd.Stderr = &loadErrors
			raw, err := cmd.Output()
			var disk struct {
				MaxConcurrentSessions int `json:"maxConcurrentSessions"`
			}
			if err != nil || json.Unmarshal(raw, &disk) != nil || disk.MaxConcurrentSessions != 0 {
				t.Fatalf("host set disk/runtime disagree: disk=%s err=%v readback=%s", raw, err, loadErrors.String())
			}
			if fixture.checkAnchors {
				cmd := exec.CommandContext(ctx, consumer, "yaml-values", path) //nolint:gosec // independent YAML parser reads the actual CLI-written config.
				cmd.Env = []string{"HOME=" + root, "PATH=/usr/bin:/bin"}
				raw, err := cmd.Output()
				var parsed struct {
					Values struct {
						Observer     int `json:"observer"`
						DefaultLimit int `json:"defaultLimit"`
					} `json:"values"`
					CapacityAnchor string `json:"capacityAnchor"`
					ObserverAlias  string `json:"observerAlias"`
					DefaultAnchor  string `json:"defaultAnchor"`
				}
				if err != nil || json.Unmarshal(raw, &parsed) != nil || parsed.Values.Observer != fixture.wantObserver ||
					parsed.ObserverAlias != "limit" || parsed.CapacityAnchor != fixture.capacityAnchor || parsed.DefaultAnchor != fixture.defaultAnchor ||
					(fixture.defaultAnchor != "" && parsed.Values.DefaultLimit != 3) {
					t.Fatalf("host set changed scalar anchor/alias semantics: parsed=%s err=%v", raw, err)
				}
			}
			capacityAssertRefused(t, live, 0)
		})
	}
}

func TestZeroCapacityReloadPreservesHeldWorker(t *testing.T) {
	afh.SkipIfShort(t, "compiled public daemon held-worker reload smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-daemon smoke")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	consumer := buildCapacityConsumer(t, source)
	live, path, root := capacityStart(t, consumer, capacityBaseYAML+"capacity: {maxConcurrentSessions: 2}\n")
	afh.RecordLive(t.Name(), afh.LiveExercised, "real daemon watcher, admission, held WorkerCommand and reaper from "+source)
	if err := capacityWait(live, 2, 0); err != nil {
		t.Fatal(err)
	}
	raw, code, err := capacityRequest(live, http.MethodPost, "/api/daemon/sessions", []byte(`{"sessionId":"capacity-first","projectId":"capacity-project","requiresRepository":false,"mode":"headless"}`))
	var first capacityHandle
	if err != nil || code != http.StatusAccepted || json.Unmarshal(raw, &first) != nil || first.PID <= 0 || first.AcceptedAt == "" {
		t.Fatalf("actual legacy admission=%d handle=%s: %v", code, raw, err)
	}
	var worker capacityWorker
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(filepath.Join(root, "capacity-first.json"))
		if err == nil && json.Unmarshal(raw, &worker) == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if worker.PID != first.PID || worker.SessionID != first.SessionID {
		t.Fatalf("actual worker identity does not match accepted handle: %+v %+v", worker, first)
	}
	ack, err := capacityWorkerCall(worker, "/ack?nonce=before-reload")
	if err != nil || ack.PID != first.PID || ack.Nonce != "before-reload" {
		t.Fatalf("initial worker acknowledgement: %+v %v", ack, err)
	}
	if err := os.WriteFile(path+".next", []byte(capacityBaseYAML+"capacity: {maxConcurrentSessions: 0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
	if err := capacityWait(live, 0, 1); err != nil {
		t.Fatal(err)
	}
	raw, code, err = capacityRequest(live, http.MethodGet, "/api/daemon/sessions", nil)
	var sessions []capacityHandle
	if err != nil || code != http.StatusOK || json.Unmarshal(raw, &sessions) != nil || len(sessions) != 1 || sessions[0] != first {
		t.Fatalf("reload changed accepted generation: first=%+v sessions=%s err=%v", first, raw, err)
	}
	ack, err = capacityWorkerCall(worker, "/ack?nonce=after-reload")
	if err != nil || ack.PID != first.PID || ack.SessionID != first.SessionID || ack.Nonce != "after-reload" {
		t.Fatalf("post-reload live worker acknowledgement: %+v %v", ack, err)
	}
	capacityAssertRefused(t, live, 1)
	if _, err := os.Stat(filepath.Join(root, "capacity-second.json")); !os.IsNotExist(err) {
		t.Fatalf("second worker artifact exists or cannot be ruled out: %v", err)
	}
	if err := capacityWait(live, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := capacityWorkerCall(worker, "/release"); err != nil {
		t.Fatal(err)
	}
	if err := capacityWait(live, 0, 0); err != nil {
		t.Fatalf("released fixture not actually reaped: %v", err)
	}
	t.Logf("same held session=%s pid=%d acceptedAt=%s acknowledged after reload; second admission refused; actual child reaped", first.SessionID, first.PID, first.AcceptedAt)
}
