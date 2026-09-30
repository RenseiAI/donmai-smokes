package smokes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// This consumer calls the public producer and registry directly; it contains
// assertions and fixture setup, not a replacement archive implementation.
const archivePublicationConsumer = `package main
import (
 "context"
 "errors"
 "fmt"
 "os"
 "path/filepath"
 "time"
 "github.com/RenseiAI/donmai/afclient"
 "github.com/RenseiAI/donmai/daemon"
)
func run() error {
 base,err:=os.MkdirTemp("","archive-publication-smoke-")
 if err!=nil{return err};defer os.RemoveAll(base)
 source:=filepath.Join(base,"source"); root:=filepath.Join(base,"archives")
 if err:=os.Mkdir(source,0700);err!=nil{return err}
 payload:=[]byte("retained Unicode 日本語\n")
 if err:=os.WriteFile(filepath.Join(source,"retained.txt"),payload,0600);err!=nil{return err}
 reader:=daemon.NewWorkareaArchiveRegistry(daemon.WorkareaArchiveOptions{Root:root})
 phases:=[]string{"after-open-archive-manifest","after-write-archive-manifest-prefix","before-publish-archive"}
 arrived:=make(chan string);release:=make(chan struct{});stop:=make(chan struct{})
 defer close(stop)
 writer:=daemon.NewWorkareaArchiveRegistry(daemon.WorkareaArchiveOptions{Root:root,ArchiveHook:func(stage string)error{
  for _,phase:=range phases{if stage==phase{select{case arrived<-stage:case <-stop:return errors.New("fixture stopped")};select{case <-release:return nil;case <-stop:return errors.New("fixture stopped")}}};return nil
 }})
 done:=make(chan error,1)
 go func(){done<-writer.ArchiveRoot(context.Background(),daemon.WorkareaRootArchiveSpec{WorkareaID:"fixture-archive",SessionID:"fixture-session",WorkareaRoot:source,SelectedPath:source})}()
 for _,phase:=range phases{
  select{case got:=<-arrived:if got!=phase{return fmt.Errorf("phase=%s want=%s",got,phase)};case err:=<-done:return fmt.Errorf("producer ended before %s: %v",phase,err);case <-time.After(15*time.Second):return fmt.Errorf("producer phase %s missing",phase)}
  entries,err:=os.ReadDir(root);if err!=nil{return err}
  stage:="";for _,entry:=range entries{if entry.IsDir(){if stage!=""{return errors.New("multiple fixture stages")};stage=entry.Name()}}
  if stage==""{return errors.New("producer stage absent")}
  _,archived,err:=reader.ListV1();if err!=nil||len(archived)!=0{return fmt.Errorf("unpublished %s leaked in list: rows=%d err=%v",phase,len(archived),err)}
  if _,err:=reader.GetV1(stage);!errors.Is(err,daemon.ErrArchiveNotFound){return fmt.Errorf("stage lookup %s: %v",phase,err)}
  if _,_,err:=reader.RestoreV1(stage,afclient.WorkareaRestoreRequest{});!errors.Is(err,daemon.ErrArchiveNotFound){return fmt.Errorf("stage restore %s: %v",phase,err)}
  release<-struct{}{}
 }
 select{case err:=<-done:if err!=nil{return err};case <-time.After(15*time.Second):return errors.New("producer publication timed out")}
 _,rows,err:=reader.ListV1();if err!=nil||len(rows)!=1||rows[0].ID!="fixture-archive"{return fmt.Errorf("published list rows=%v err=%v",rows,err)}
 archive,err:=reader.GetV1("fixture-archive");if err!=nil{return err}
 got,err:=os.ReadFile(filepath.Join(archive.WorkareaRoot,"retained.txt"));if err!=nil||string(got)!=string(payload){return fmt.Errorf("published tree changed: %v",err)}
 restored,_,err:=reader.RestoreV1("fixture-archive",afclient.WorkareaRestoreRequest{});if err!=nil{return err}
 got,err=os.ReadFile(filepath.Join(restored.WorkareaRoot,"retained.txt"));if err!=nil||string(got)!=string(payload){return fmt.Errorf("restored tree changed: %v",err)}
 got,err=os.ReadFile(filepath.Join(source,"retained.txt"));if err!=nil||string(got)!=string(payload){return fmt.Errorf("source tree changed: %v",err)}
 fmt.Println("PASS: real producer empty/partial/complete stages withheld; final list/get/restore and source bytes retained")
 return nil
}
func main(){if err:=run();err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
`

func TestArchivePublicationThroughPublicConsumer(t *testing.T) {
	afh.SkipIfShort(t, "compile-and-run public archive producer smoke")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
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
		t.Fatal("producer source has no Go directive")
	}
	dir := t.TempDir()
	module := fmt.Sprintf("module archive-publication-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\nreplace github.com/RenseiAI/donmai => %s\n", version, source)
	for name, body := range map[string]string{"go.mod": module, "main.go": archivePublicationConsumer} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "consumer")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-race", "-o", binary, "."}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // test-owned module and fixed Go arguments.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("consumer go %v: %v\n%s", args, err, output)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary) //nolint:gosec // exact test-built executable.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "GOWORK=off"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual producer consumer: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS: real producer empty/partial/complete stages withheld") {
		t.Fatalf("consumer evidence missing: %s", output)
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "public ArchiveRoot writer and independent List/Get/Restore registry")
	t.Log(strings.TrimSpace(string(output)))
}
