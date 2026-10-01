package smokes

// This drives the unmodified CLI and its local runtime. Only the external
// GitHub API, gh CLI and native vendor protocol are finite private fixtures.
// It is not evidence of a real account, model entitlement or live GitHub.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const (
	profileFixtureToken = "synthetic-profile-github-token"
	profileVendorSource = `package main
import("bytes";"crypto/sha256";"encoding/json";"fmt";"io";"net/http";"os";"os/exec";"path/filepath";"strconv";"strings";"time")
func root()string{p,e:=os.Executable();if e!=nil{panic(e)};return filepath.Dir(filepath.Dir(p))}
func record(v any){b,e:=json.Marshal(v);if e!=nil{panic(e)};f,e:=os.OpenFile(filepath.Join(root(),"vendor.jsonl"),os.O_APPEND|os.O_CREATE|os.O_WRONLY,0600);if e!=nil{panic(e)};defer f.Close();if _,e=f.Write(append(b,'\n'));e!=nil{panic(e)}}
func git(dir string,args ...string)string{c:=exec.Command("git",append([]string{"-c","core.hooksPath=/dev/null"},args...)...);c.Dir=dir;b,e:=c.CombinedOutput();if e!=nil{stage:="githead";if len(args)>0&&args[0]=="symbolic-ref"{stage="gitbranch"};record(map[string]any{"kind":"failure-stage","stage":stage,"ok":false});panic(fmt.Sprintf("owned git: %v: %s",e,b))};return strings.TrimSpace(string(b))}
func api(method,path string,body any)map[string]any{var b bytes.Buffer;if body!=nil{if e:=json.NewEncoder(&b).Encode(body);e!=nil{panic(e)}};r,e:=http.NewRequest(method,"https://api.github.com"+path,&b);if e!=nil{panic(e)};r.Header.Set("Authorization","Bearer "+os.Getenv("GITHUB_TOKEN"));r.Header.Set("Content-Type","application/json");c:=&http.Client{Timeout:5*time.Second};resp,e:=c.Do(r);if e!=nil{panic(e)};defer resp.Body.Close();raw,e:=io.ReadAll(io.LimitReader(resp.Body,1<<20));if e!=nil||resp.StatusCode!=200&&resp.StatusCode!=201{panic(fmt.Sprintf("fixture external API status %d: %s",resp.StatusCode,raw))};var v map[string]any;if e=json.Unmarshal(raw,&v);e!=nil{panic(e)};return v}
func gh(){a:=os.Args[1:];record(map[string]any{"kind":"gh","args":a});if len(a)<2||a[0]!="pr"{panic("unexpected external gh operation")};switch a[1]{case "create":head,base:="","";for i:=2;i+1<len(a);i++{switch a[i]{case "--head":head=a[i+1];case "--base":base=a[i+1]}};if head==""||base!="release/next"{panic("PR does not name exact session head and configured base")};v:=api("POST","/repos/example/project/pulls",map[string]any{"head":head,"base":base});fmt.Println(v["html_url"]);case "view":if len(a)<3||a[2]!="https://github.com/example/project/pull/101"{panic("unexpected PR readback")};v:=api("GET","/repos/example/project/pulls/101",nil);h:=v["head"].(map[string]any);b:=v["base"].(map[string]any);_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"number":101,"url":v["html_url"],"headRefName":h["ref"],"baseRefName":b["ref"]});default:panic("unexpected external gh verb")}}
func workerContract(a []string)bool{if len(a)<3||a[1]!="agent"||a[2]!="run"{return false};local,contracts:=false,0;version:="";for i:=3;i<len(a);i++{switch{case a[i]=="--local-runtime":local=true;case a[i]=="--local-runtime=true":local=true;case strings.HasPrefix(a[i],"--local-runtime="):return false;case a[i]=="--local-runtime-contract":if i+1>=len(a){return false};i++;contracts++;version=a[i];case strings.HasPrefix(a[i],"--local-runtime-contract="):contracts++;version=strings.TrimPrefix(a[i],"--local-runtime-contract=")}};return local&&contracts==1&&version=="local/v2"}
type nativeSession struct{ID string;CWD string;Branch string;Head string}
func resumeNative(a []string,cwd,workerSHA string,workerPID int)bool{resume:=false;for _,arg:=range a{if arg=="--resume"||strings.HasPrefix(arg,"--resume="){resume=true}};if !resume{return false};want:=[]string{"-p","--output-format","stream-json","--verbose","--dangerously-skip-permissions","--resume","synthetic-native-stream"};if strings.Join(a,"\x00")!=strings.Join(want,"\x00"){record(map[string]any{"kind":"failure-stage","stage":"resume-flags","ok":false});panic("resume protocol flags or session identity mismatch")};raw,e:=os.ReadFile(filepath.Join(root(),"native-session.json"));if e!=nil{panic("resume has no persisted initial native session")};var saved nativeSession;if e=json.Unmarshal(raw,&saved);e!=nil{panic("invalid private native session")};branch,head:=git(cwd,"symbolic-ref","--quiet","HEAD"),git(cwd,"rev-parse","HEAD");if saved.ID!=want[6]||saved.CWD!=cwd||saved.Branch!=branch||saved.Head==""{record(map[string]any{"kind":"failure-stage","stage":"resume-session","ok":false});panic("resume changed owned native session identity")};if head!=saved.Head{git(cwd,"merge-base","--is-ancestor",saved.Head,head)};prompt,e:=io.ReadAll(io.LimitReader(os.Stdin,(1<<20)+1));if e!=nil||len(prompt)>1<<20||len(bytes.TrimSpace(prompt))==0{record(map[string]any{"kind":"failure-stage","stage":"resume-stdin","ok":false});panic("resume requires finite nonempty stdin prompt")};record(map[string]any{"kind":"resume","worker_sha256":workerSHA,"worker_pid":workerPID,"same_session":true,"same_cwd":true,"same_branch":true,"head_descends":true});fmt.Println("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"synthetic-native-stream\"}");fmt.Println("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"WORK_RESULT:passed\\n\"}]}}");fmt.Println("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1}");return true}
func claude(){a:=os.Args[1:];if len(a)==1&&a[0]=="--version"{record(map[string]any{"kind":"version"});fmt.Println("2.1.197 (Claude Code)");return};if len(a)==3&&a[0]=="auth"&&a[1]=="status"&&a[2]=="--json"{record(map[string]any{"kind":"login"});fmt.Println("{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\"}");return};record(map[string]any{"kind":"spawn-enter","pid":os.Getpid(),"ppid":os.Getppid()});joined:=strings.Join(a," ");isResume:=false;for _,arg:=range a{if arg=="--resume"||strings.HasPrefix(arg,"--resume="){isResume=true}};if !isResume&&(!strings.Contains(joined,"stream-json")||!strings.Contains(joined,"claude-sonnet-5")){record(map[string]any{"kind":"failure-stage","stage":"native-argv-model","ok":false});panic("native protocol missing actual adapted model/output arguments")};cwd,e:=os.Getwd();if e!=nil{record(map[string]any{"kind":"failure-stage","stage":"cwd","ok":false});panic(e)};if !strings.HasPrefix(cwd,filepath.Join(root(),"home",".donmai","worktrees")+string(os.PathSeparator)){record(map[string]any{"kind":"failure-stage","stage":"cwd-boundary","ok":false});panic("native work is not in private owned worktree")};parent:=os.Getppid();f,e:=os.Open("/proc/"+strconv.Itoa(parent)+"/exe");if e!=nil{record(map[string]any{"kind":"failure-stage","stage":"proc-parent-exe","ok":false});panic(e)};h:=sha256.New();if _,e=io.Copy(h,f);e!=nil{record(map[string]any{"kind":"failure-stage","stage":"proc-parent-exe-read","ok":false});panic(e)};_ = f.Close();cmd,e:=os.ReadFile("/proc/"+strconv.Itoa(parent)+"/cmdline");if e!=nil{record(map[string]any{"kind":"failure-stage","stage":"proc-parent-workerflags-read","ok":false});panic(e)};workerArgs:=strings.Split(strings.TrimRight(string(cmd),"\x00"),"\x00");if !workerContract(workerArgs){record(map[string]any{"kind":"failure-stage","stage":"proc-parent-workerflags","ok":false});panic("native parent is not shipped local/v2 worker")};if resumeNative(a,cwd,fmt.Sprintf("%x",h.Sum(nil)),parent){return};record(map[string]any{"kind":"stream","pid":os.Getpid(),"worker_pid":parent,"worker_command":"agent/run","worker_transport":"local/v2","worker_sha256":fmt.Sprintf("%x",h.Sum(nil)),"cwd":cwd,"head":git(cwd,"rev-parse","HEAD"),"branch":git(cwd,"symbolic-ref","--quiet","HEAD"),"adapted_native_stream":true});fmt.Println("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"synthetic-native-stream\"}");deadline:=time.Now().Add(40*time.Second);for{if _,e=os.Stat(filepath.Join(root(),"release-native"));e==nil{break};if time.Now().After(deadline){panic("finite native release was not received")};time.Sleep(20*time.Millisecond)};if e=os.WriteFile(filepath.Join(cwd,"profile-proof.txt"),[]byte("private native-protocol work\n"),0600);e!=nil{panic(e)};saved,e:=json.Marshal(nativeSession{ID:"synthetic-native-stream",CWD:cwd,Branch:git(cwd,"symbolic-ref","--quiet","HEAD"),Head:git(cwd,"rev-parse","HEAD")});if e!=nil{panic(e)};if e=os.WriteFile(filepath.Join(root(),"native-session.json"),saved,0600);e!=nil{panic(e)};fmt.Println("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"WORK_RESULT:passed\\n\"}]}}");fmt.Println("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1}")}
func main(){switch filepath.Base(os.Args[0]){case "claude":claude();case "gh":gh();default:panic("unknown external fixture identity")}}
`
)

type profileGitHub struct {
	mu              sync.Mutex
	remote, baseSHA string
	labeled         bool
	pr              map[string]any
	comments        []map[string]any
	requests        map[string]int
	lostComment     bool
	errors          []string
	verifyReady     bool
}

func profileGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...) //nolint:gosec // fixed private Git fixture.
	cmd.Dir = dir
	cmd.Env = env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("private Git %v: %v: %s", args, err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func (g *profileGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	g.requests[key]++
	fail := func(message string) {
		g.errors = append(g.errors, message)
		http.Error(w, message, http.StatusBadRequest)
	}
	if r.Header.Get("Authorization") != "Bearer "+profileFixtureToken {
		fail("external fixture authentication mismatch")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		write(map[string]any{"id": 77, "login": "fixture-user"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project":
		write(map[string]any{"id": 42, "full_name": "example/project", "default_branch": "release/next"})
	case r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/example/project/branches/release%2Fnext":
		write(map[string]any{"name": "release/next", "commit": map[string]any{"sha": g.baseSHA}})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/issues":
		if r.URL.Query().Get("labels") != "work-ready" || r.URL.Query().Get("state") != "open" {
			fail("intake query lost configured eligibility")
			return
		}
		if !g.labeled {
			write([]any{})
			return
		}
		write([]any{g.issue(), g.issue()})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/issues/7":
		write(g.issue())
	case r.Method == http.MethodPost && r.URL.Path == "/repos/example/project/issues/7/labels":
		var body struct {
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Labels) != 1 || body.Labels[0] != "work-ready" {
			fail("authored CLI label arguments changed")
			return
		}
		g.labeled = true
		write([]any{map[string]any{"name": "work-ready"}})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/example/project/pulls":
		var body struct{ Head, Base string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Base != "release/next" || !strings.HasPrefix(body.Head, "donmai/session_") {
			fail("PR creation lost configured new-branch identity")
			return
		}
		cmd := exec.Command("git", "--git-dir", g.remote, "rev-parse", "refs/heads/"+body.Head) //nolint:gosec // fixture validates private remote and session branch prefix.
		raw, err := cmd.Output()
		if err != nil {
			fail("PR head was not actually pushed")
			return
		}
		head := strings.TrimSpace(string(raw))
		if head == g.baseSHA {
			fail("PR head has no actual work commit")
			return
		}
		if err = exec.Command("git", "--git-dir", g.remote, "update-ref", "refs/pull/101/head", head).Run(); err != nil {
			fail("cannot expose fixture GitHub pull ref")
			return
		} //nolint:gosec // private Git remote only.
		repo := map[string]any{"id": 42}
		g.pr = map[string]any{"number": 101, "html_url": "https://github.com/example/project/pull/101", "head": map[string]any{"sha": head, "ref": body.Head, "repo": repo}, "base": map[string]any{"ref": body.Base, "repo": repo}}
		w.WriteHeader(http.StatusCreated)
		write(g.pr)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/example/project/pulls/101":
		if g.pr == nil {
			http.NotFound(w, r)
			return
		}
		if !g.verifyReady {
			altered := map[string]any{}
			for k, v := range g.pr {
				altered[k] = v
			}
			head := map[string]any{}
			for k, v := range g.pr["head"].(map[string]any) {
				head[k] = v
			}
			head["sha"] = strings.Repeat("0", 40)
			altered["head"] = head
			write(altered)
			return
		}
		write(g.pr)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/example/project/issues/"), "/comments")
		if target != "7" && target != "101" {
			fail("foreign comment read")
			return
		}
		result := []any{}
		for _, c := range g.comments {
			if c["issue_url"] == "https://api.github.com/repos/example/project/issues/"+target {
				result = append(result, c)
			}
		}
		write(result)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/example/project/issues/"), "/comments")
		if target != "7" && target != "101" {
			fail("foreign comment write")
			return
		}
		var body struct{ Body string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Body, "<!-- donmai-local-publication:") {
			fail("publication has no actual durable receipt marker")
			return
		}
		id := 10001 + len(g.comments)
		kind := "issues"
		if target == "101" {
			kind = "pull"
		}
		comment := map[string]any{"id": id, "body": body.Body, "user": map[string]any{"id": 77}, "issue_url": "https://api.github.com/repos/example/project/issues/" + target, "html_url": "https://github.com/example/project/" + kind + "/" + target + "#issuecomment-" + strconv.Itoa(id)}
		g.comments = append(g.comments, comment)
		if !g.lostComment {
			g.lostComment = true
			h, ok := w.(http.Hijacker)
			if !ok {
				fail("TLS fixture cannot simulate owned lost response")
				return
			}
			conn, _, err := h.Hijack()
			if err != nil {
				fail("cannot drop owned comment response")
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusCreated)
		write(comment)
	default:
		fail("unexpected external API " + key)
	}
}

func (g *profileGitHub) issue() map[string]any {
	labels := []any{}
	if g.labeled {
		labels = append(labels, map[string]any{"name": "work-ready"})
	}
	return map[string]any{"number": 7, "title": "Private local issue", "body": "Write the private proof file and finish the authored change.", "state": "open", "html_url": "https://github.com/example/project/issues/7", "labels": labels}
}

func profileTLSProxy(t *testing.T, root string, g *profileGitHub) (proxyURL, caFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "private profile fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"api.github.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}
	api := httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	api.StartTLS()
	t.Cleanup(api.Close)
	caFile = filepath.Join(root, "fixture-ca.pem")
	if err = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.github.com:443" {
			http.Error(w, "foreign proxy destination refused", http.StatusForbidden)
			return
		}
		target, err := net.DialTimeout("tcp", api.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "fixture TLS target unavailable", http.StatusServiceUnavailable)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = target.Close()
			return
		}
		if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = client.Close()
			_ = target.Close()
			return
		}
		if err = buffer.Flush(); err != nil {
			_ = client.Close()
			_ = target.Close()
			return
		}
		go func() { _, _ = io.Copy(target, client); _ = target.Close() }()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		_ = target.Close()
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, caFile
}

func profileCLI(t *testing.T, ctx context.Context, binary, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // compiled SUT and explicit fixture args.
	cmd.Dir = dir
	cmd.Env = env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual CLI %v: %v: %s", args, err, raw)
	}
	return string(raw)
}

func profileSetup(t *testing.T, binary, root string, env []string, rerun bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	answers := []string{"profile-fixture", "private", "y", "1", "256", "1", "y", "2", "y", "1", "example/project", "work-ready", "release/next", "y", "", "manual", "3"}
	if rerun {
		answers = []string{"", "", "y", "", "", "", "y", "", "y", "", "", "", "", "y", "", "", ""}
	}
	// Linux script supplies a real PTY; no wizard override or hand-written config.
	command := "exec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' host setup"
	cmd := exec.CommandContext(ctx, "script", "-qefc", command, "/dev/null")
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdin = strings.NewReader(strings.Join(answers, "\n") + "\n")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual PTY setup (rerun=%v): %v: %s", rerun, err, raw)
	}
	if !strings.Contains(string(raw), "Setup complete.") {
		t.Fatalf("real wizard did not finish: %s", raw)
	}
	return string(raw)
}

func profileRecords(t *testing.T, root string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "vendor.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	result := []map[string]any{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var v map[string]any
		if err = json.Unmarshal(line, &v); err != nil {
			t.Fatalf("fixture observation JSON: %v", err)
		}
		result = append(result, v)
	}
	return result
}

type profileProcess struct {
	PID, PPID, PGID int
	Start           string
	State           string
}

type profileWorkerWitness struct {
	Daemon, Worker      profileProcess
	SessionID, Worktree string
}

func profileProc(pid int) (profileProcess, error) {
	if pid <= 1 {
		return profileProcess{}, fmt.Errorf("invalid owned PID")
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return profileProcess{}, err
	}
	if len(raw) > 4096 {
		return profileProcess{}, fmt.Errorf("procstat exceeds bound")
	}
	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen < 0 {
		return profileProcess{}, fmt.Errorf("invalid procstat")
	}
	fields := strings.Fields(string(raw[closeParen+1:]))
	if len(fields) < 20 {
		return profileProcess{}, fmt.Errorf("short procstat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return profileProcess{}, err
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return profileProcess{}, err
	}
	return profileProcess{PID: pid, PPID: ppid, PGID: pgid, Start: fields[19], State: fields[0]}, nil
}

func profileWithin(boundary, path string) bool {
	relative, err := filepath.Rel(boundary, filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func profileBindWorker(binary, boundary string, daemon profileProcess, rows []map[string]any) (*profileWorkerWitness, error) {
	current, err := profileProc(daemon.PID)
	if err != nil || current.Start != daemon.Start {
		return nil, fmt.Errorf("owned daemon identity unavailable")
	}
	for _, row := range rows {
		value, ok := row["pid"].(float64)
		if !ok || value <= 1 {
			continue
		}
		worker, err := profileProc(int(value))
		if err != nil || worker.PPID != daemon.PID {
			continue
		}
		exe, err := os.Stat("/proc/" + strconv.Itoa(worker.PID) + "/exe")
		if err != nil {
			continue
		}
		known, err := os.Stat(binary)
		if err != nil || !os.SameFile(exe, known) {
			continue
		}
		path, ok := row["worktreePath"].(string)
		if !ok || !profileWithin(boundary, path) {
			continue
		}
		id, ok := row["sessionId"].(string)
		if !ok || !strings.HasPrefix(id, "session_") {
			continue
		}
		after, err := profileProc(worker.PID)
		if err != nil || after.Start != worker.Start || after.PPID != worker.PPID {
			continue
		}
		return &profileWorkerWitness{Daemon: daemon, Worker: worker, SessionID: id, Worktree: path}, nil
	}
	return nil, fmt.Errorf("no indexed direct same-artifact worker identity available")
}

// children is per-task, not per-TGID. Aggregate only bounded tasks belonging
// to this already identity-checked process. This remains a non-atomic witness;
// absence is not proof that every child was absent or quiescent.
func profileTaskChildren(process profileProcess) (children []profileProcess, threads int, truncated bool, err error) {
	before, err := profileProc(process.PID)
	if err != nil || before.Start != process.Start || before.PPID != process.PPID {
		return nil, 0, false, fmt.Errorf("parent identity changed before task scan")
	}
	base := "/proc/" + strconv.Itoa(process.PID) + "/task"
	dir, err := os.Open(base)
	if err != nil {
		return nil, 0, false, err
	}
	entries, readErr := dir.ReadDir(65)
	_ = dir.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, 0, false, readErr
	}
	if len(entries) > 64 {
		entries = entries[:64]
		truncated = true
	}
	seen := map[int]bool{}
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil || tid <= 1 {
			continue
		}
		task := base + "/" + strconv.Itoa(tid)
		file, err := os.Open(task + "/status")
		if err != nil {
			continue
		}
		status, readErr := io.ReadAll(io.LimitReader(file, 16<<10))
		_ = file.Close()
		if readErr != nil {
			continue
		}
		tgid := 0
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Tgid:") {
				tgid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Tgid:")))
				break
			}
		}
		if tgid != process.PID {
			continue
		}
		current, err := profileProc(process.PID)
		if err != nil || current.Start != process.Start || current.PPID != process.PPID {
			return nil, threads, truncated, fmt.Errorf("parent identity changed during task scan")
		}
		threads++
		file, err = os.Open(task + "/children")
		if err != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 4097))
		_ = file.Close()
		if readErr != nil || len(raw) > 4096 {
			truncated = true
			continue
		}
		for _, text := range strings.Fields(string(raw)) {
			if len(children) >= 32 {
				truncated = true
				break
			}
			pid, err := strconv.Atoi(text)
			if err != nil || seen[pid] {
				continue
			}
			seen[pid] = true
			child, err := profileProc(pid)
			if err != nil || child.PPID != process.PID {
				continue
			}
			parentStart, parentErr := strconv.ParseUint(process.Start, 10, 64)
			childStart, childErr := strconv.ParseUint(child.Start, 10, 64)
			if parentErr != nil || childErr != nil || childStart < parentStart {
				continue
			}
			children = append(children, child)
		}
	}
	after, err := profileProc(process.PID)
	if err != nil || after.Start != process.Start || after.PPID != process.PPID {
		return nil, threads, truncated, fmt.Errorf("parent identity changed after task scan")
	}
	return children, threads, truncated, nil
}

func profileProcessSnapshots(t *testing.T, boundary string, witness *profileWorkerWitness) {
	t.Helper()
	if witness == nil {
		t.Log("failure process witness unavailable: no earlier actual indexed worker identity")
		return
	}
	for snapshot := 0; snapshot < 2; snapshot++ {
		daemon, err := profileProc(witness.Daemon.PID)
		if err != nil || daemon.Start != witness.Daemon.Start {
			t.Log("failure process witness refused: daemon identity changed")
			return
		}
		worker, err := profileProc(witness.Worker.PID)
		if err != nil || worker.Start != witness.Worker.Start || worker.PPID != daemon.PID {
			t.Log("failure process witness refused: indexed worker gone/reused/reparented")
			return
		}
		type owned struct {
			process profileProcess
			parent  profileProcess
			depth   int
		}
		pending := []owned{{worker, daemon, 0}}
		seen := map[int]bool{}
		records := []map[string]any{}
		for len(pending) > 0 && len(records) < 32 {
			item := pending[0]
			pending = pending[1:]
			if seen[item.process.PID] {
				continue
			}
			seen[item.process.PID] = true
			parent, err := profileProc(item.parent.PID)
			if err != nil || parent.Start != item.parent.Start {
				continue
			}
			current, err := profileProc(item.process.PID)
			if err != nil || current.Start != item.process.Start || current.PPID != item.process.PPID {
				continue
			}
			base := "/proc/" + strconv.Itoa(current.PID)
			record := map[string]any{"pid": current.PID, "ppid": current.PPID, "pgid": current.PGID, "starttime": current.Start, "state": current.State, "depth": item.depth}
			for _, name := range []string{"comm", "wchan"} {
				raw, err := os.ReadFile(base + "/" + name)
				if err == nil && len(raw) <= 256 {
					record[name] = strings.TrimSpace(string(raw))
				}
			}
			if exe, err := os.Readlink(base + "/exe"); err == nil {
				record["executable_basename"] = filepath.Base(exe)
			}
			if cwd, err := os.Readlink(base + "/cwd"); err == nil {
				if profileWithin(boundary, cwd) {
					relative, _ := filepath.Rel(boundary, cwd)
					record["cwd_fixture_relative"] = relative
				} else {
					record["cwd_category"] = "outside fixture (unreported)"
				}
			}
			after, err := profileProc(current.PID)
			parentAfter, parentErr := profileProc(item.parent.PID)
			if parentErr != nil || parentAfter.Start != item.parent.Start || err != nil || after.Start != current.Start || after.PPID != current.PPID {
				continue
			}
			records = append(records, record)
			if item.depth >= 8 {
				record["children_truncated_by_depth"] = true
				continue
			}
			children, threads, truncated, err := profileTaskChildren(current)
			record["owned_tasks_observed"] = threads
			record["task_children_non_atomic"] = true
			if truncated {
				record["task_children_scan_truncated"] = true
			}
			if err != nil {
				record["task_children_identity_uncertain"] = true
				continue
			}
			for _, child := range children {
				if len(pending)+len(records) >= 32 {
					record["children_truncated_by_count"] = true
					break
				}
				pending = append(pending, owned{child, current, item.depth + 1})
			}
		}
		raw, err := json.Marshal(records)
		if err == nil {
			t.Logf("failure owned process snapshot%d metadata-only: %s", snapshot+1, raw)
		}
		if snapshot == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	// Paths from the index are only predicted; inspect the actual private leaf.
	if !profileWithin(boundary, witness.Worktree) {
		t.Log("failure private Git metadata refused: path outside fixture")
		return
	}
	if real, err := filepath.EvalSymlinks(witness.Worktree); err == nil && !profileWithin(boundary, real) {
		t.Log("failure private Git metadata refused: actual worktree escapes fixture")
		return
	}
	paths := []string{witness.Worktree, filepath.Join(witness.Worktree, ".git")}
	if agent, err := filepath.EvalSymlinks(filepath.Join(witness.Worktree, ".agent")); err == nil && profileWithin(boundary, agent) {
		paths = append(paths, filepath.Join(agent, "state.json"))
	}
	gitRoot := filepath.Join(witness.Worktree, ".git")
	if info, err := os.Lstat(gitRoot); err == nil && info.Mode().IsRegular() {
		raw, err := os.ReadFile(gitRoot)
		if err == nil && len(raw) <= 4096 && strings.HasPrefix(string(raw), "gitdir: ") {
			target := strings.TrimSpace(strings.TrimPrefix(string(raw), "gitdir: "))
			if !filepath.IsAbs(target) {
				target = filepath.Join(witness.Worktree, target)
			}
			if real, err := filepath.EvalSymlinks(target); err == nil && profileWithin(boundary, real) {
				gitRoot = real
			} else {
				gitRoot = ""
			}
		}
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		gitRoot = ""
	}
	if gitRoot != "" {
		for _, name := range []string{"HEAD", "index", "index.lock", "shallow", "packed-refs", "FETCH_HEAD", "objects"} {
			paths = append(paths, filepath.Join(gitRoot, name))
		}
	}
	metadata := []map[string]any{}
	for _, path := range paths {
		relative, _ := filepath.Rel(boundary, path)
		entry := map[string]any{"fixture_relative": relative}
		info, err := os.Lstat(path)
		if err != nil {
			entry["exists"] = false
		} else {
			entry["exists"] = true
			entry["mode"] = info.Mode().String()
			entry["size"] = info.Size()
			entry["mtime_unix_nano"] = info.ModTime().UnixNano()
		}
		metadata = append(metadata, entry)
	}
	if gitRoot != "" {
		head := filepath.Join(gitRoot, "HEAD")
		if info, err := os.Lstat(head); err == nil && info.Mode().IsRegular() && info.Size() <= 256 {
			raw, err := os.ReadFile(head)
			if err == nil {
				value := strings.TrimSpace(string(raw))
				classification := "detached or other (value unreported)"
				if strings.HasPrefix(value, "ref: refs/heads/") {
					classification = "symbolic other (name unreported)"
					if value == "ref: refs/heads/donmai/"+witness.SessionID {
						classification = "symbolic expected session branch"
					}
					if value == "ref: refs/heads/release/next" {
						classification = "symbolic configured base"
					}
				}
				t.Logf("failure actual private HEAD classification: %s", classification)
			}
		}
	}
	if raw, err := json.Marshal(metadata); err == nil {
		t.Logf("failure private worktree/Git metadata only: %s", raw)
	}
}

// Failure evidence is collected before the owned stop and TempDir cleanups.
// Only this fixture's log/index/projection and vendor operation kinds are read;
// no protected record or native credential contents are printed.
func profileFailureDiagnostics(t *testing.T, root, home, origin, logPath string, client *http.Client, witness *profileWorkerWitness) {
	t.Helper()
	if !t.Failed() {
		return
	}
	profileProcessSnapshots(t, filepath.Dir(root), witness)
	private := struct {
		OperatorSecret string `json:"operatorSecret"`
	}{}
	if raw, err := os.ReadFile(filepath.Join(home, ".donmai", "queue.auth", "bootstrap.json")); err == nil {
		_ = json.Unmarshal(raw, &private)
	}
	secrets := regexp.MustCompile(`(?:locop_|locat_)[A-Za-z0-9_-]+`)
	redact := func(raw []byte) string {
		text := strings.ReplaceAll(string(raw), profileFixtureToken, "[REDACTED]")
		if private.OperatorSecret != "" {
			text = strings.ReplaceAll(text, private.OperatorSecret, "[REDACTED]")
		}
		return secrets.ReplaceAllString(text, "[REDACTED]")
	}
	if file, err := os.Open(logPath); err == nil {
		if info, err := file.Stat(); err == nil {
			if info.Size() > 32<<10 {
				_, _ = file.Seek(info.Size()-(32<<10), io.SeekStart)
			}
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 32<<10))
		_ = file.Close()
		t.Logf("failure actual daemon log tail (bounded/redacted): %s; read=%v", redact(raw), readErr)
	} else {
		t.Logf("failure daemon log unavailable: %v", err)
	}
	counts := map[string]int{}
	if file, err := os.Open(filepath.Join(root, "vendor.jsonl")); err == nil {
		decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
		for {
			var observation struct {
				Kind  string `json:"kind"`
				Stage string `json:"stage"`
			}
			err := decoder.Decode(&observation)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Logf("failure vendor kind scan incomplete: %v", err)
				break
			}
			counts[observation.Kind]++
			if observation.Kind == "failure-stage" {
				switch observation.Stage {
				case "native-argv-model", "cwd", "cwd-boundary", "proc-parent-exe", "proc-parent-exe-read", "proc-parent-workerflags-read", "proc-parent-workerflags", "githead", "gitbranch", "resume-flags", "resume-session", "resume-stdin":
					counts["failure-stage/"+observation.Stage]++
				}
			}
		}
		_ = file.Close()
	}
	t.Logf("failure actual vendor operation kind counts: %v", counts)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/daemon/sessions", nil)
	if err != nil {
		t.Logf("failure index request unavailable: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("failure actual session index unavailable: %v", err)
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	_ = resp.Body.Close()
	if len(raw) > 16<<10 {
		raw = raw[:16<<10]
	}
	t.Logf("failure actual session index status=%d bounded/redacted=%s; read=%v", resp.StatusCode, redact(raw), readErr)
	var rows []struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &rows) != nil || private.OperatorSecret == "" {
		return
	}
	for i, row := range rows {
		if i >= 4 {
			t.Log("failure projection diagnostics limited to four actual index rows")
			break
		}
		if row.SessionID == "" {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/daemon/local/sessions/"+row.SessionID, nil)
		if err != nil {
			continue
		}
		request.Header.Set("Authorization", "Bearer "+private.OperatorSecret)
		response, err := client.Do(request)
		if err != nil {
			t.Logf("failure actual protected projection unavailable: %v", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
		_ = response.Body.Close()
		if len(body) > 16<<10 {
			body = body[:16<<10]
		}
		t.Logf("failure actual protected projection status=%d bounded/redacted=%s; read=%v", response.StatusCode, redact(body), readErr)
	}
}

func profileWait(t *testing.T, ctx context.Context, description string, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not converge: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Read only this fixture's protected generated operator key. It is used for
// the real public projection endpoint and is never printed or persisted anew.
func profileProjection(t *testing.T, ctx context.Context, client *http.Client, origin, home, sessionID string) map[string]any {
	t.Helper()
	path := filepath.Join(home, ".donmai", "queue.auth", "bootstrap.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatal("fixture operator metadata is not private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		OperatorSecret string `json:"operatorSecret"`
	}
	if err = json.Unmarshal(raw, &record); err != nil || record.OperatorSecret == "" {
		t.Fatal("fixture generated operator authority missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/daemon/local/sessions/"+sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+record.OperatorSecret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real local session projection status=%d", resp.StatusCode)
	}
	var projection map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&projection); err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), record.OperatorSecret) {
		t.Fatal("operator credential leaked to public session projection")
	}
	return projection
}

func TestLocalGitHubWholeRuntime(t *testing.T) {
	afh.SkipIfShort(t, "compiled whole local runtime smoke")
	afh.SkipIfKnob(t, "DONMAI_SMOKES_SKIP_LIVE_DAEMON", "foreground local runtime smoke")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	if _, err := os.Stat(filepath.Join(source, "afcli", "local_runtime.go")); err != nil {
		t.Fatalf("located source lacks local runtime composition: %v", err)
	}
	if runtime.GOOS != "linux" {
		afh.DeclineLive(t, "private fixed-origin CA/PTY/proc identity proof requires isolated Linux; system trust is not modified")
	}
	for _, tool := range []string{"git", "script"} {
		afh.SkipIfToolMissing(t, tool, "private whole-runtime fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: source})
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	vendorModule := filepath.Join(root, "vendor-module")
	if err := os.MkdirAll(vendorModule, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorModule, "go.mod"), []byte("module private-profile-vendor\n\ngo 1.26.6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorModule, "main.go"), []byte(profileVendorSource), 0o600); err != nil {
		t.Fatal(err)
	}
	vendor := filepath.Join(bin, "vendor-fixture")
	build := exec.CommandContext(ctx, "go", "build", "-o", vendor, ".")
	build.Dir = vendorModule
	build.Env = append(os.Environ(), "GOWORK=off")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build external protocol fixture: %v: %s", err, raw)
	}
	for _, name := range []string{"claude", "gh"} {
		if err := os.Symlink(vendor, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + root, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "NO_COLOR=1", "TERM=dumb", "GITHUB_TOKEN=" + profileFixtureToken, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "git-seed")
	profileGit(t, env, "", "init", "--bare", remote)
	profileGit(t, env, "", "init", "-b", "main", seed)
	profileGit(t, env, seed, "config", "user.name", "Fixture")
	profileGit(t, env, seed, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profileGit(t, env, seed, "add", "source.txt")
	profileGit(t, env, seed, "commit", "-m", "main")
	mainSHA := profileGit(t, env, seed, "rev-parse", "HEAD")
	profileGit(t, env, seed, "remote", "add", "origin", remote)
	profileGit(t, env, seed, "push", "origin", "main")
	profileGit(t, env, seed, "checkout", "-b", "release/next")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("configured nondefault base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profileGit(t, env, seed, "commit", "-am", "selected base")
	profileGit(t, env, seed, "push", "origin", "release/next")
	baseSHA := profileGit(t, env, seed, "rev-parse", "HEAD")
	profileGit(t, env, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	config := filepath.Join(root, "gitconfig")
	configText := "[user]\n name = Fixture\n email = fixture@example.invalid\n[core]\n hooksPath = /dev/null\n[url \"file://" + remote + "\"]\n insteadOf = https://github.com/example/project.git\n[protocol \"file\"]\n allow = always\n"
	if err := os.WriteFile(config, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "GIT_CONFIG_GLOBAL="+config)
	external := &profileGitHub{remote: remote, baseSHA: baseSHA, requests: map[string]int{}}
	proxy, ca := profileTLSProxy(t, root, external)
	certDir := filepath.Join(root, "empty-cert-dir")
	if err := os.Mkdir(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env = append(env, "HTTPS_PROXY="+proxy, "HTTP_PROXY="+proxy, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+certDir)
	first := profileSetup(t, binary, root, env, false)
	second := profileSetup(t, binary, root, env, true)
	cfgPath := filepath.Join(home, ".donmai", "daemon.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"donmai.dev/v2", "claude-sonnet-5", "example/project", "work-ready", "release/next"} {
		if !strings.Contains(string(cfg), wanted) {
			t.Fatalf("actual wizard config lost %q: %s", wanted, cfg)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	var done chan error
	var daemonIdentity profileProcess
	var workerWitness *profileWorkerWitness
	stopped := true
	quiescenceUnknown := false
	start := func() {
		daemonCmd := exec.Command(binary, "host", "run", "--port", strconv.Itoa(port), "--standalone-creds", "on")
		daemonCmd.Dir = root
		daemonCmd.Env = env
		daemonCmd.Stdout = logFile
		daemonCmd.Stderr = logFile
		if err := daemonCmd.Start(); err != nil {
			t.Fatal(err)
		}
		identity, err := profileProc(daemonCmd.Process.Pid)
		if err != nil {
			t.Logf("owned daemon diagnostic identity unavailable: %v", err)
		}
		daemonIdentity = identity
		done = make(chan error, 1)
		waiter := done
		go func() { waiter <- daemonCmd.Wait() }()
		stopped = false
	}
	start()
	stop := func() {
		if stopped || quiescenceUnknown {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer stopCancel()
		cmd := exec.CommandContext(stopCtx, binary, "host", "stop", "--port", strconv.Itoa(port))
		cmd.Dir = root
		cmd.Env = env
		raw, stopErr := cmd.CombinedOutput()
		if stopErr != nil {
			t.Errorf("owned foreground stop: %v: %s", stopErr, raw)
		}
		select {
		case err := <-done:
			stopped = true
			if err != nil {
				t.Errorf("owned daemon exit: %v", err)
			}
		case <-stopCtx.Done():
			quiescenceUnknown = true
			t.Error("owned foreground daemon exit unproved; exact container cleanup must retain responsibility")
		}
	}
	t.Cleanup(stop)
	origin := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: time.Second}
	getSessions := func() []map[string]any {
		resp, err := client.Get(origin + "/api/daemon/sessions")
		if err != nil {
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			return nil
		}
		var rows []map[string]any
		if err = json.NewDecoder(resp.Body).Decode(&rows); err != nil {
			return nil
		}
		return rows
	}
	// Registered after stop/TempDir: LIFO runs diagnostics while the actual
	// daemon and owned fixture files are still available on a failed assertion.
	t.Cleanup(func() { profileFailureDiagnostics(t, root, home, origin, logPath, client, workerWitness) })
	profileWait(t, ctx, "actual daemon health", func() bool {
		r, err := client.Get(origin + "/healthz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 200
	})
	// Observe at least two genuine eligible-source polls before changing a label.
	profileWait(t, ctx, "unlabeled source baseline", func() bool {
		external.mu.Lock()
		defer external.mu.Unlock()
		return external.requests["GET /repos/example/project/issues"] >= 2
	})
	if rows := getSessions(); len(rows) != 0 {
		t.Fatalf("unlabeled issue became a session: %v", rows)
	}
	profileCLI(t, ctx, binary, root, env, "github", "add-labels", "--repo", "example/project", "--number", "7", "--labels", "work-ready")
	var stream map[string]any
	profileWait(t, ctx, "actual native stream behind shipped worker", func() bool {
		if workerWitness == nil {
			if observed, err := profileBindWorker(binary, filepath.Dir(root), daemonIdentity, getSessions()); err == nil {
				workerWitness = observed
			}
		}
		for _, r := range profileRecords(t, root) {
			if r["kind"] == "stream" {
				stream = r
				return true
			}
		}
		return false
	})
	sutBytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sutSHA := fmt.Sprintf("%x", sha256.Sum256(sutBytes))
	if stream["worker_sha256"] != sutSHA || stream["head"] != baseSHA {
		t.Fatalf("actual worker artifact/base mismatch: %v", stream)
	}
	branch := strings.TrimPrefix(stream["branch"].(string), "refs/heads/")
	if !strings.HasPrefix(branch, "donmai/session_") {
		t.Fatalf("native stream amended a base branch: %q", branch)
	}
	sessionID := strings.TrimPrefix(branch, "donmai/")
	rows := getSessions()
	if len(rows) != 1 || rows[0]["sessionId"] != sessionID {
		t.Fatalf("one actual source/session not visible: %v", rows)
	}
	if rows[0]["repository"] != "https://github.com/example/project.git" || rows[0]["harness"] != "claude-code" || rows[0]["model"] != "claude-sonnet-5" || rows[0]["state"] != "running" {
		t.Fatalf("actual live index identity changed: %v", rows)
	}
	statsResponse, err := client.Get(origin + "/api/daemon/stats")
	if err != nil {
		t.Fatal(err)
	}
	var liveWorker struct {
		WorkerID string `json:"workerId"`
	}
	statsErr := json.NewDecoder(io.LimitReader(statsResponse.Body, 1<<20)).Decode(&liveWorker)
	_ = statsResponse.Body.Close()
	if statsResponse.StatusCode != http.StatusOK || statsErr != nil || liveWorker.WorkerID == "" {
		t.Fatalf("actual worker identity unavailable: status=%d decode=%v", statsResponse.StatusCode, statsErr)
	}
	statePath := filepath.Join(stream["cwd"].(string), ".agent", "state.json")
	stateFile, err := os.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var liveState struct {
		SessionID       string `json:"sessionId"`
		IssueIdentifier string `json:"issueIdentifier"`
		Harness         string `json:"harness"`
		Model           string `json:"model"`
	}
	stateErr := json.NewDecoder(io.LimitReader(stateFile, 1<<20)).Decode(&liveState)
	_ = stateFile.Close()
	if stateErr != nil || liveState.SessionID != sessionID || liveState.IssueIdentifier != "example/project#7" || liveState.Harness != rows[0]["harness"] || liveState.Model != rows[0]["model"] {
		t.Fatalf("actual owned state/index identity mismatch: %+v; decode=%v", liveState, stateErr)
	}
	watchCtx, watchCancel := context.WithTimeout(ctx, time.Second)
	watch := exec.CommandContext(watchCtx, binary, "host", "watch", "--all", "--plain", "--daemon-url", origin)
	watch.Dir = root
	watch.Env = env
	watchRaw, _ := watch.CombinedOutput()
	watchCancel()
	// Plain cards render the actual state-derived issue identifier, not the
	// full session ID. Full identity remains bound above to index and state.
	for _, identity := range []string{liveState.IssueIdentifier, "project example/project", "harness " + liveState.Harness, "model " + liveState.Model, "state running"} {
		if !strings.Contains(string(watchRaw), identity) {
			t.Fatalf("actual piped hostwatch omitted bound identity %q: %s", identity, watchRaw)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "release-native"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profileWait(t, ctx, "actual completed result held behind independent wrong-head readback", func() bool {
		external.mu.Lock()
		havePR := external.pr != nil
		reads := external.requests["GET /repos/example/project/pulls/101"]
		external.mu.Unlock()
		if !havePR || reads < 2 {
			return false
		}
		projection := profileProjection(t, ctx, client, origin, home, sessionID)
		if projection["terminalStatus"] != "completed" {
			return false
		}
		publications, ok := projection["publications"].([]any)
		if !ok || len(publications) != 1 {
			return false
		}
		pub, ok := publications[0].(map[string]any)
		if !ok || pub["state"] != "pending" {
			t.Fatal("unverified head escaped pending publication")
		}
		external.mu.Lock()
		posts := external.requests["POST /repos/example/project/issues/101/comments"]
		external.mu.Unlock()
		if posts != 0 {
			t.Fatal("publisher wrote receipt before independent head match")
		}
		return true
	})
	external.mu.Lock()
	external.verifyReady = true
	external.mu.Unlock()
	profileWait(t, ctx, "independent actual terminal publications", func() bool {
		external.mu.Lock()
		defer external.mu.Unlock()
		return external.pr != nil && len(external.comments) == 1 && external.requests["GET /repos/example/project/pulls/101"] >= 2
	})
	var terminalProjection map[string]any
	profileWait(t, ctx, "durable terminal and publication projection", func() bool {
		terminalProjection = profileProjection(t, ctx, client, origin, home, sessionID)
		publications, ok := terminalProjection["publications"].([]any)
		if terminalProjection["terminalStatus"] != "completed" || !ok || len(publications) != 1 {
			return false
		}
		pub, ok := publications[0].(map[string]any)
		return ok && pub["state"] == "delivered" && pub["headSha"] != "" && pub["readAt"] != ""
	})
	// Completion must survive duplicate issue polls and the intentionally lost
	// first comment response. The remote GET reconciliation, not a repeated POST,
	// is what completes that durable publication.
	external.mu.Lock()
	prRaw, _ := json.Marshal(external.pr)
	commentsRaw, _ := json.Marshal(external.comments)
	requestSnapshot := map[string]int{}
	for k, v := range external.requests {
		requestSnapshot[k] = v
	}
	remoteErrors := append([]string(nil), external.errors...)
	external.mu.Unlock()
	if len(remoteErrors) > 0 {
		t.Fatalf("external protocol contract failures: %v", remoteErrors)
	}
	if requestSnapshot["POST /repos/example/project/pulls"] != 1 || requestSnapshot["POST /repos/example/project/issues/7/comments"] != 0 || requestSnapshot["POST /repos/example/project/issues/101/comments"] != 1 {
		t.Fatalf("duplicate actual publication effects: %v", requestSnapshot)
	}
	if !strings.Contains(string(commentsRaw), "Result receipt SHA-256:") || !strings.Contains(string(commentsRaw), "Verified PR head:") || !strings.Contains(string(commentsRaw), sessionID) {
		t.Fatalf("authenticated durable result/PR receipts missing: %s", commentsRaw)
	}
	if got := profileGit(t, env, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); got != mainSHA {
		t.Fatal("whole runtime changed remote main")
	}
	if got := profileGit(t, env, "", "--git-dir", remote, "rev-parse", "refs/heads/release/next"); got != baseSHA {
		t.Fatal("whole runtime amended configured base")
	}
	stop()
	if !stopped {
		t.Fatal("cannot restart an unproved exited daemon")
	}
	external.mu.Lock()
	pollsBeforeRestart := external.requests["GET /repos/example/project/issues"]
	external.mu.Unlock()
	start()
	profileWait(t, ctx, "actual restarted local source polls", func() bool {
		external.mu.Lock()
		defer external.mu.Unlock()
		return external.requests["GET /repos/example/project/issues"] >= pollsBeforeRestart+2
	})
	replayed := profileProjection(t, ctx, client, origin, home, sessionID)
	if replayed["terminalStatus"] != "completed" {
		t.Fatalf("real restart lost durable terminal: %v", replayed)
	}
	external.mu.Lock()
	replayCreates := external.requests["POST /repos/example/project/pulls"]
	replayComments := external.requests["POST /repos/example/project/issues/101/comments"]
	external.mu.Unlock()
	if replayCreates != 1 || replayComments != 1 {
		t.Fatal("real restart duplicated independently observed publication")
	}
	stop()
	queue := filepath.Join(home, ".donmai", "queue")
	admissions, terminals, published := 0, 0, 0
	var receiptBody []byte
	var receiptSHA string
	if err = filepath.WalkDir(queue, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var tx struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &tx) != nil {
			return nil
		}
		switch tx.Kind {
		case "admit":
			admissions++
		case "terminal":
			terminals++
			var terminal struct {
				Receipt struct {
					Attempt struct {
						ScopeID    string `json:"scopeId"`
						SessionID  string `json:"sessionId"`
						AttemptID  string `json:"attemptId"`
						WorkerID   string `json:"workerId"`
						Generation uint64 `json:"generation"`
					} `json:"attempt"`
					Body       []byte `json:"body"`
					Status     string `json:"status"`
					BodySHA256 string `json:"bodySha256"`
				} `json:"receipt"`
				Event struct {
					Type       string `json:"type"`
					ScopeID    string `json:"scopeId"`
					SessionID  string `json:"sessionId"`
					Status     string `json:"status"`
					BodySHA256 string `json:"bodySha256"`
				} `json:"event"`
			}
			if err := json.Unmarshal(tx.Data, &terminal); err != nil {
				return err
			}
			attempt, event := terminal.Receipt.Attempt, terminal.Event
			if attempt.SessionID != sessionID || event.SessionID != sessionID || attempt.ScopeID == "" || event.ScopeID != attempt.ScopeID || attempt.WorkerID != liveWorker.WorkerID || attempt.AttemptID == "" || attempt.Generation == 0 {
				return fmt.Errorf("actual durable terminal attempt/event identity mismatch")
			}
			if terminal.Receipt.Status != "completed" || event.Status != "completed" || event.Type != "session.ended" {
				return fmt.Errorf("actual durable terminal is not completed session.ended")
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(terminal.Receipt.Body))
			if terminal.Receipt.BodySHA256 != digest || event.BodySHA256 != digest {
				return fmt.Errorf("actual durable terminal exact body digest mismatch")
			}
			receiptBody = terminal.Receipt.Body
			receiptSHA = terminal.Receipt.BodySHA256
		case "publication":
			published++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if admissions != 1 || terminals != 1 || published != 1 {
		t.Fatalf("actual durable queue effects admissions=%d terminals=%d publication=%d", admissions, terminals, published)
	}
	var terminalBody map[string]any
	if err = json.Unmarshal(receiptBody, &terminalBody); err != nil {
		t.Fatalf("actual durable worker result: %v", err)
	}
	var actualPR struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err = json.Unmarshal(prRaw, &actualPR); err != nil {
		t.Fatal(err)
	}
	actualHead := profileGit(t, env, "", "--git-dir", remote, "rev-parse", "refs/heads/"+branch)
	if actualPR.Head.SHA != actualHead || terminalBody["commitSha"] != actualHead || terminalBody["workerId"] != liveWorker.WorkerID || terminalBody["status"] != "completed" || terminalBody["pullRequestUrl"] != "https://github.com/example/project/pull/101" {
		t.Fatalf("durable terminal correlation changed: %v", terminalBody)
	}
	if receiptSHA == "" || !strings.Contains(string(commentsRaw), "Result receipt SHA-256: "+receiptSHA) {
		t.Fatal("remote authenticated comment does not bind actual durable receipt SHA")
	}
	streamCount, resumeCount := 0, 0
	for _, r := range profileRecords(t, root) {
		if r["kind"] == "stream" {
			streamCount++
		}
		if r["kind"] == "resume" {
			resumeCount++
			if r["worker_sha256"] != sutSHA || r["worker_pid"] != stream["worker_pid"] {
				t.Fatal("native resume changed the actual same-artifact worker")
			}
		}
		if r["kind"] == "failure-stage" {
			t.Fatal("external native protocol validation failed")
		}
	}
	if resumeCount == 0 {
		t.Fatal("actual native session resume was not exercised")
	}
	if streamCount != 1 {
		t.Fatalf("duplicate source polls spawned%d native streams", streamCount)
	}
	projectionBytes, err := json.Marshal(terminalProjection)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(projectionBytes), string(receiptBody), first, second, string(watchRaw), string(prRaw), string(commentsRaw), string(cfg)} {
		if strings.Contains(raw, profileFixtureToken) {
			t.Fatal("fixture credential leaked to public metadata or output")
		}
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "actual CLI wizard/label/local-v2 same-artifact worker/runner/queue/publisher/hostwatch with TLS-verified fixed-origin external fixtures")
}
