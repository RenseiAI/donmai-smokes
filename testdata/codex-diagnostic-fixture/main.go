// Command codex-diagnostic-fixture is a local peer for the CLI diagnostic smoke.
package main

import (
 "context"
 "encoding/json"
 "fmt"
 "net"
 "net/http"
 "os"
 "os/signal"
 "path/filepath"
 "strings"
 "syscall"
 "time"

 "github.com/coder/websocket"
)

func main() { os.Exit(run()) }

func run() int {
 if len(os.Args) == 2 && os.Args[1] == "--version" {
  fmt.Println("codex-cli 0.154.0")
  return 0
 }
 if len(os.Args) == 4 && strings.Join(os.Args[1:], " ") == "mcp list --json" {
  fmt.Println("[]")
  return 0
 }
 if len(os.Args) < 2 || os.Args[1] != "app-server" { return 2 }
 exe, err := os.Executable()
 if err != nil { return 2 }
 dir := filepath.Dir(exe)
 mode, err := os.ReadFile(filepath.Join(dir, "mode"))
 if err != nil { return 2 }
 trace := func(event string) {
  f, err := os.OpenFile(filepath.Join(dir, "trace"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
  if err != nil { return }
  _, _ = fmt.Fprintln(f, event)
  _ = f.Close()
 }
 diagnostic := func() {
  _, _ = fmt.Fprintln(os.Stderr, "fatal: named Codex diagnostic fixture\nAuthorization: Bearer sk-smoke-diagnostic-not-a-real-key")
 }
 stopped := make(chan os.Signal, 1)
 signal.Notify(stopped, syscall.SIGTERM)
 defer signal.Stop(stopped)
 socket := ""
 for i, arg := range os.Args {
  if arg == "--listen" && i+1 < len(os.Args) { socket = strings.TrimPrefix(os.Args[i+1], "unix://") }
 }
 if socket == "" { return 2 }
 listener, err := net.Listen("unix", socket)
 if err != nil { return 3 }
 defer listener.Close()
 handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  conn, err := websocket.Accept(w, r, nil)
  if err != nil { return }
  defer conn.CloseNow()
  ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
  defer cancel()
  _, data, err := conn.Read(ctx)
  if err != nil { return }
  var req struct {
   ID json.RawMessage `json:"id"`
   Method string `json:"method"`
   Params struct { ClientInfo struct { Title string `json:"title"` } `json:"clientInfo"` } `json:"params"`
  }
  if json.Unmarshal(data, &req) != nil || req.Method != "initialize" || req.Params.ClientInfo.Title != "Donmai Interactive Name Bootstrap" {
   trace("unexpected-request")
   return
  }
  trace("named-initialize")
  if string(mode) == "before-response" { diagnostic() }
  response, _ := json.Marshal(map[string]any{"jsonrpc":"2.0", "id":req.ID, "result":map[string]string{"codexHome":"deliberately-wrong-config-home"}})
  if conn.Write(ctx, websocket.MessageText, response) != nil { return }
  _, _, _ = conn.Read(ctx)
 })
 srv := &http.Server{Handler:handler, ReadHeaderTimeout:5*time.Second}
 defer srv.Close()
 go func() { _ = srv.Serve(listener) }()
 select {
 case <-stopped:
  if string(mode) == "shutdown" { diagnostic() }
  trace("terminated")
  return 0
 case <-time.After(30*time.Second):
  trace("expired")
  return 4
 }
}
