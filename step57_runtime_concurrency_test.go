package smokes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestRuntimeConcurrencyPublicConsumer builds a separate module against the
// selected source, then drives only public Pool and Worktree Manager APIs.
func TestRuntimeConcurrencyPublicConsumer(t *testing.T) {
	afh.SkipIfShort(t, "compiled public runtime-concurrency consumer")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	afh.SkipIfToolMissing(t, "go", "external runtime consumer requires the Go toolchain")
	afh.SkipIfToolMissing(t, "git", "registration exclusion requires real local Git")
	moduleBytes, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
			break
		}
	}
	if version == "" {
		t.Fatal("selected source module has no Go directive")
	}
	moduleDir := t.TempDir()
	mod := fmt.Sprintf("module runtime-concurrency-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0\nreplace github.com/RenseiAI/donmai => %s\n", version, strconv.Quote(source))
	for name, body := range map[string]string{"go.mod": mod, "consumer_test.go": step57RuntimeConsumer} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "-v", "-mod=mod", ".")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("public runtime consumer from %s failed: %v\n%s", source, err, output)
	}
	for _, name := range []string{"TestPublicPoolExpiredWarmDeadline", "TestPublicPoolCanceledTriggerKeepsWaiter", "TestPublicManagerExcludesRegistrationWriter", "TestPublicManagerWaitingFetchTimeoutAndRecovery", "TestPublicManagerDistinctFetchesOverlap"} {
		if !strings.Contains(string(output), "--- PASS: "+name+" ") {
			t.Fatalf("public consumer did not execute %s:\n%s", name, output)
		}
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled selected-source external public Pool/Manager consumer with owned Git registration and deadline controls")
	t.Logf("public runtime consumer source=%s:\n%s", source, output)
}

const step57RuntimeConsumer = `package consumer_test

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"

 "github.com/RenseiAI/donmai/runtime/codeintelhost"
 mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
 "github.com/RenseiAI/donmai/runtime/worktree"
)

type warmCaller struct{}
func(warmCaller)WaitReady(context.Context)error{return nil}
func(warmCaller)Call(_ context.Context,name string,_ json.RawMessage)(mcpserver.ToolResult,error){return mcpserver.ToolResult{Content:[]mcpserver.ContentItem{{Type:"text",Text:name}}},nil}
type warmFactory struct{calls atomic.Int32;entered chan struct{};release chan struct{};once sync.Once}
func(f *warmFactory)Create(ctx context.Context,_ codeintelhost.Binding)(codeintelhost.ToolCaller,io.Closer,error){
 f.calls.Add(1);f.once.Do(func(){close(f.entered)})
 select{case <-f.release:return warmCaller{},io.NopCloser(strings.NewReader("")),nil;case <-ctx.Done():return nil,nil,ctx.Err()}
}
func binding()codeintelhost.Binding{return codeintelhost.Binding{OrgID:"fixture-org",ProjectID:"fixture-project",RepositoryPathID:"fixture-repo",RevisionKind:codeintelhost.RevisionResolvedRef,Revision:strings.Repeat("a",40)}}
type observedContext struct{context.Context;entered chan struct{};once sync.Once}
func(c *observedContext)Done()<-chan struct{}{c.once.Do(func(){close(c.entered)});return c.Context.Done()}

func TestPublicPoolExpiredWarmDeadline(t *testing.T){
 f:=&warmFactory{entered:make(chan struct{}),release:make(chan struct{})};close(f.release)
 pool,err:=codeintelhost.NewPool(f,codeintelhost.PoolConfig{MaxWorkareas:1});if err!=nil{t.Fatal(err)}
 b:=binding();held,err:=pool.Acquire(context.Background(),b);if err!=nil{t.Fatal(err)};t.Cleanup(held.Release)
 expired,cancel:=context.WithDeadline(context.Background(),time.Unix(1,0));t.Cleanup(cancel)
 if !errors.Is(expired.Err(),context.DeadlineExceeded){t.Fatal("fixture deadline is not settled")}
 for attempt:=0;attempt<64;attempt++{lease,err:=pool.Acquire(expired,b);if lease!=nil{lease.Release()};if lease!=nil || !errors.Is(err,context.DeadlineExceeded){t.Fatalf("expired warm caller acquired lease: attempt=%d lease=%v error=%v",attempt,lease!=nil,err)}}
 other:=b;other.Revision=strings.Repeat("b",40)
 unexpected,err:=pool.Acquire(context.Background(),other);if unexpected!=nil{unexpected.Release()};if !errors.Is(err,codeintelhost.ErrAtCapacity){t.Fatalf("expired caller released healthy hold: %v",err)}
 healthy,err:=pool.Acquire(context.Background(),b);if err!=nil{t.Fatal(err)};healthy.Release()
 if f.calls.Load()!=1{t.Fatalf("factory calls=%d, want single warm",f.calls.Load())}
}

func TestPublicPoolCanceledTriggerKeepsWaiter(t *testing.T){
 f:=&warmFactory{entered:make(chan struct{}),release:make(chan struct{})}
 pool,err:=codeintelhost.NewPool(f,codeintelhost.PoolConfig{MaxWorkareas:1});if err!=nil{t.Fatal(err)}
 short,cancel:=context.WithTimeout(context.Background(),10*time.Millisecond);t.Cleanup(cancel)
 type result struct{lease *codeintelhost.Lease;err error}
 trigger:=make(chan result,1);go func(){lease,err:=pool.Acquire(short,binding());trigger<-result{lease,err}}()
 <-f.entered
 background:=&observedContext{Context:context.Background(),entered:make(chan struct{})}
 survivor:=make(chan result,1);go func(){lease,err:=pool.Acquire(background,binding());survivor<-result{lease,err}}()
 <-background.entered
 first:=<-trigger;if first.lease!=nil{first.lease.Release()};if first.lease!=nil || !errors.Is(first.err,context.DeadlineExceeded){close(f.release);t.Fatalf("trigger result lease=%v error=%v",first.lease!=nil,first.err)}
 close(f.release);second:=<-survivor;if second.err!=nil || second.lease==nil{t.Fatalf("healthy shared waiter aborted: %v",second.err)};second.lease.Release()
 if f.calls.Load()!=1{t.Fatalf("factory calls=%d, want one shared warm",f.calls.Load())}
}

type gitFixture struct{root,parent,seed,base,tip string;env []string}
func newGitFixture(t *testing.T)*gitFixture{
 t.Helper();root:=t.TempDir();f:=&gitFixture{root:root,parent:filepath.Join(root,"parent"),seed:filepath.Join(root,"seed")}
 home:=filepath.Join(root,"home");if err:=os.MkdirAll(home,0o700);err!=nil{t.Fatal(err)}
 f.env=[]string{"PATH="+os.Getenv("PATH"),"HOME="+home,"GIT_CONFIG_NOSYSTEM=1","GIT_CONFIG_GLOBAL="+filepath.Join(root,"empty-config"),"GIT_TERMINAL_PROMPT=0"}
 bare:=filepath.Join(root,"remote.git")
 f.git(t,"init","--bare","--initial-branch=main",bare);f.git(t,"init","--initial-branch=main",f.seed)
 f.git(t,"-C",f.seed,"config","user.name","Fixture");f.git(t,"-C",f.seed,"config","user.email","fixture@example.com")
 if err:=os.WriteFile(filepath.Join(f.seed,"source.txt"),[]byte("initial\n"),0o600);err!=nil{t.Fatal(err)}
 f.git(t,"-C",f.seed,"add","source.txt");f.git(t,"-C",f.seed,"commit","-m","initial fixture")
 f.base=f.git(t,"-C",f.seed,"rev-parse","HEAD")
 f.git(t,"-C",f.seed,"branch","release/one");f.git(t,"-C",f.seed,"branch","release/two")
 f.git(t,"-C",f.seed,"remote","add","origin",bare);f.git(t,"-C",f.seed,"push","origin","main","release/one","release/two")
 f.git(t,"clone","--branch","main",bare,f.parent)
 if err:=os.WriteFile(filepath.Join(f.seed,"source.txt"),[]byte("advanced\n"),0o600);err!=nil{t.Fatal(err)}
 f.git(t,"-C",f.seed,"commit","-am","advance fixture")
 f.tip=f.git(t,"-C",f.seed,"rev-parse","HEAD")
 f.git(t,"-C",f.seed,"push","origin","HEAD:refs/heads/release/one","HEAD:refs/heads/release/two")
 return f
}
func(f *gitFixture)run(ctx context.Context,name string,args ...string)([]byte,error){
 if name!="git"{return nil,fmt.Errorf("unexpected fixture command %q",name)}
 command:=exec.CommandContext(ctx,"git",args...);command.Env=f.env;return command.CombinedOutput()
}
func(f *gitFixture)git(t *testing.T,args ...string)string{t.Helper();ctx,cancel:=context.WithTimeout(t.Context(),5*time.Second);defer cancel();out,err:=f.run(ctx,"git",args...);if err!=nil{t.Fatalf("real fixture git %v: %v\n%s",args,err,out)};return strings.TrimSpace(string(out))}
func(f *gitFixture)registration(t *testing.T)string{
 t.Helper();admin:=filepath.Join(f.parent,".git","worktrees","owned-null-registration");checkout:=filepath.Join(f.root,"owned-registration")
 for _,dir:=range []string{admin,checkout}{if err:=os.MkdirAll(dir,0o700);err!=nil{t.Fatal(err)}}
 head:=filepath.Join(admin,"HEAD")
 for path,body:=range map[string]string{head:strings.Repeat("0",len(f.base))+"\n",filepath.Join(admin,"gitdir"):filepath.Join(checkout,".git")+"\n",filepath.Join(admin,"commondir"):"../..\n",filepath.Join(checkout,".git"):"gitdir: "+admin+"\n"}{if err:=os.WriteFile(path,[]byte(body),0o600);err!=nil{t.Fatal(err)}}
 return head
}
func newManager(t *testing.T,f *gitFixture,leaf string,timeout time.Duration,runner worktree.CommandRunner)*worktree.Manager{
 t.Helper();m,err:=worktree.NewManager(worktree.Options{ParentDir:filepath.Join(f.root,leaf),CommandRunner:runner,BaseFetchTimeout:timeout});if err!=nil{t.Fatal(err)};return m
}
func provisionSpec(f *gitFixture,session,ref string)worktree.ProvisionSpec{return worktree.ProvisionSpec{SessionID:session,Branch:"work/"+session,BaseRef:"origin/"+ref,Strategy:worktree.StrategyWorktreeAdd,ParentRepoPath:f.parent}}
func isFetch(args []string)bool{return len(args)>2 && args[0]=="-C" && args[2]=="fetch"}

type writerGate struct{entered,release chan struct{};head string;once sync.Once;result chan error}
func startWriter(t *testing.T,f *gitFixture,ctx context.Context)*writerGate{
 t.Helper();g:=&writerGate{entered:make(chan struct{}),release:make(chan struct{}),result:make(chan error,1)}
 writer:=newManager(t,f,"writer-areas",time.Second,func(ctx context.Context,name string,args ...string)([]byte,error){
  if len(args)>3 && args[2]=="worktree" && args[3]=="add"{g.head=f.registration(t);close(g.entered);select{case <-g.release:case <-ctx.Done():return nil,ctx.Err()}}
  return f.run(ctx,name,args...)
 })
 spec:=provisionSpec(f,"writer","release/one");spec.SkipBaseFetch=true
 go func(){_,err:=writer.Provision(ctx,spec);g.result<-err}()
 select{case <-g.entered:case err:=<-g.result:t.Fatalf("writer did not reach actual worktree add: %v",err);case <-ctx.Done():t.Fatal("writer-entry timeout")}
 t.Cleanup(func(){g.restore(t,f)})
 return g
}
func(g *writerGate)restore(t *testing.T,f *gitFixture){t.Helper();g.once.Do(func(){if err:=os.WriteFile(g.head,[]byte(f.base+"\n"),0o600);err!=nil{t.Error(err)};close(g.release)})}
func(g *writerGate)finish(t *testing.T,ctx context.Context){t.Helper();select{case err:=<-g.result:if err!=nil{t.Fatalf("real writer failed: %v",err)};case <-ctx.Done():t.Fatal("writer completion timeout")}}

func TestPublicManagerExcludesRegistrationWriter(t *testing.T){
 f:=newGitFixture(t);ctx,cancel:=context.WithTimeout(t.Context(),5*time.Second);t.Cleanup(cancel)
 writer:=startWriter(t,f,ctx)
 entered:=make(chan struct{});var once sync.Once
 reader:=newManager(t,f,"reader-areas",time.Second,func(ctx context.Context,name string,args ...string)([]byte,error){if isFetch(args){once.Do(func(){close(entered)})};return f.run(ctx,name,args...)})
 observed:=&observedContext{Context:ctx,entered:make(chan struct{})}
 result:=make(chan error,1);go func(){_,err:=reader.Provision(observed,provisionSpec(f,"reader","release/one"));result<-err}()
 select{case <-observed.entered:case <-ctx.Done():t.Fatal("reader never joined public fetch wait")}
 select{
 case <-entered:
  // Preserve the null HEAD until the actual Git fetch reports its failure.
  select{case err:=<-result:if err==nil || !strings.Contains(err.Error(),"bad object worktrees/owned-null-registration/HEAD"){t.Fatalf("fetch entered writer without actual null-HEAD connectivity witness: %v",err)};t.Fatalf("actual fetch crossed registration writer: %v",err);case <-ctx.Done():t.Fatal("fetch crossed writer and did not finish")}
 case <-time.After(300*time.Millisecond):
 case <-ctx.Done():t.Fatal("writer-exclusion observation timeout")
 }
 writer.restore(t,f)
 select{case err:=<-result:if err!=nil{t.Fatal(err)};case <-ctx.Done():t.Fatal("reader did not resume")}
 writer.finish(t,ctx)
 actual,err:=reader.Result("reader");if err!=nil{t.Fatal(err)};if !actual.BaseFetched || actual.BaseSHA!=f.tip{t.Fatalf("reader base not advanced: fetched=%v sha=%s want=%s",actual.BaseFetched,actual.BaseSHA,f.tip)}
 if f.git(t,"-C",f.parent,"rev-parse","origin/release/one")!=f.tip{t.Fatal("actual remote tracking ref did not advance")}
}

func TestPublicManagerWaitingFetchTimeoutAndRecovery(t *testing.T){
 f:=newGitFixture(t);ctx,cancel:=context.WithTimeout(t.Context(),5*time.Second);t.Cleanup(cancel)
 writer:=startWriter(t,f,ctx);var fetches atomic.Int32
 runner:=func(ctx context.Context,name string,args ...string)([]byte,error){if isFetch(args){fetches.Add(1)};return f.run(ctx,name,args...)}
 reader:=newManager(t,f,"timeout-areas",25*time.Millisecond,runner)
 _,err:=reader.Provision(ctx,provisionSpec(f,"timed-out","release/one"))
 if !errors.Is(err,context.DeadlineExceeded) || !errors.Is(err,worktree.ErrBaseFetch){t.Fatalf("held writer did not bound fetch wait: %v",err)}
 if fetches.Load()!=0{t.Fatalf("expired fetch reached Git %d times",fetches.Load())}
 if _,err:=reader.Result("timed-out");!errors.Is(err,worktree.ErrUnknownSession){t.Fatalf("expired reader retained successful session: %v",err)}
 writer.restore(t,f);writer.finish(t,ctx)
 // The expired flight is shared by canonical parent/ref across managers.
 // Recover against that same key and workarea directory using the normal real-Git budget.
 recovery:=newManager(t,f,"timeout-areas",time.Second,runner)
 _,err=recovery.Provision(ctx,provisionSpec(f,"recovered","release/one"));if err!=nil{t.Fatalf("canceled flight prevented recovery: %v",err)}
 actual,err:=recovery.Result("recovered");if err!=nil{t.Fatal(err)};if actual.BaseSHA!=f.tip || !actual.BaseFetched || fetches.Load()!=1{t.Fatalf("recovery did not clean flight and advance ref: result=%+v fetches=%d",actual,fetches.Load())}
 if f.git(t,"-C",f.parent,"rev-parse","origin/release/one")!=f.tip{t.Fatal("recovery remote tracking ref did not advance")}
}

func TestPublicManagerDistinctFetchesOverlap(t *testing.T){
 f:=newGitFixture(t);ctx,cancel:=context.WithTimeout(t.Context(),5*time.Second);t.Cleanup(cancel)
 entered:=make(chan string,2);release:=make(chan struct{});var once sync.Once;t.Cleanup(func(){once.Do(func(){close(release)})})
 runner:=func(ctx context.Context,name string,args ...string)([]byte,error){if isFetch(args){entered<-args[len(args)-1];select{case <-release:case <-ctx.Done():return nil,ctx.Err()}};return f.run(ctx,name,args...)}
 first:=newManager(t,f,"distinct-one",time.Second,runner);second:=newManager(t,f,"distinct-two",time.Second,runner)
 results:=make(chan error,2)
 go func(){_,err:=first.Provision(ctx,provisionSpec(f,"one","release/one"));results<-err}()
 go func(){_,err:=second.Provision(ctx,provisionSpec(f,"two","release/two"));results<-err}()
 refs:=map[string]bool{}
 for len(refs)<2{select{case ref:=<-entered:if refs[ref]{t.Fatalf("duplicate fetch ref=%s",ref)};refs[ref]=true;case <-ctx.Done():t.Fatal("distinct ref fetches were serialized instead of overlapping")}}
 once.Do(func(){close(release)})
 for count:=0;count<2;count++{select{case err:=<-results:if err!=nil{t.Fatalf("real overlapping fetch/provision: %v",err)};case <-ctx.Done():t.Fatal("distinct provisions did not finish")}}
 for _,pair:=range []struct{manager *worktree.Manager;session string}{{first,"one"},{second,"two"}}{actual,err:=pair.manager.Result(pair.session);if err!=nil{t.Fatal(err)};if !actual.BaseFetched || actual.BaseSHA!=f.tip{t.Fatalf("distinct fetch base=%s want=%s",actual.BaseSHA,f.tip)}}
}
`
