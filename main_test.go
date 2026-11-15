package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, mode string) (*App, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Error("accept")
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "example-session")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "example-session" {
				t.Error("session")
			}
			sc := any(map[string]any{"type": "object"})
			if mode == "schema" {
				sc = map[string]any{"type": "array"}
			}
			result = map[string]any{"tools": []any{map[string]any{"name": "example_read", "inputSchema": sc}}}
		case "tools/call":
			calls++
			if mode == "auth" || (mode == "page-auth" && calls == 2) {
				w.WriteHeader(401)
				return
			}
			if mode == "fetch" {
				w.WriteHeader(500)
				return
			}
			if mode == "deadline" {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Second):
					return
				}
			}
			if mode == "rpc" {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-1,"message":"secret"}}`, req.ID)
				return
			}
			if mode == "disconnect" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\"")
				return
			}
			if mode == "missing" {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d}`, req.ID)
				return
			}
			target := map[string]any{"items": []any{json.Number("9007199254740993123456789")}, "next": nil}
			if mode == "declared-alternating" && calls%2 == 0 {
				delete(target, "next")
			}
			if mode == "truncate" {
				target["items"] = []any{1, 2}
			}
			if mode == "pages" || mode == "page-auth" || mode == "cycle" {
				if calls == 1 || mode == "cycle" {
					target["next"] = "example-next"
				}
			}
			if mode == "shape" {
				target["extra"] = true
			}
			result = map[string]any{"structuredContent": target, "content": []any{map[string]any{"type": "text", "text": "ignored"}}}
			if mode == "isError" {
				result.(map[string]any)["isError"] = true
			}
			if mode == "text" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"items":[],"next":null}`}}}
			}
			if mode == "sse" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":", req.ID)
				b, _ := json.Marshal(result)
				_, _ = fmt.Fprintf(w, "%s}\n\n", b)
				return
			}
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		b, _ := json.Marshal(result)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, b)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("EXAMPLE_ENDPOINT", srv.URL)
	t.Setenv("EXAMPLE_TOKEN", "example-secret")
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	cfg := Config{RawDir: filepath.Join(root, "raw"), Deadline: "500ms", MaxAge: "15m", Shadow: ".", Servers: map[string]Server{"example-server": {EndpointEnv: "EXAMPLE_ENDPOINT", TokenEnv: "EXAMPLE_TOKEN", Tools: map[string]Policy{"example_read": {SchemaHash: hashValue(map[string]any{"name": "example_read", "inputSchema": map[string]any{"type": "object"}}), Args: map[string][]any{"limit": {json.Number("2")}}, Required: []string{"limit"}, Page: &Page{Items: "items", Next: "next", LimitArg: "limit", CursorArg: "cursor", MaxPages: 3}}}}}}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return &App{ConfigPath: path}, &calls
}
func callArgs() []string {
	return []string{"call", "example-server", "example_read", "--args", `{"limit":2}`, "--expect", `.items|type=="array"`, "--jq", ".items[]"}
}
func run(t *testing.T, a *App, args []string, want int) Envelope {
	t.Helper()
	e, c := a.Run(args)
	if c != want {
		t.Fatalf("code=%d want=%d meta=%+v", c, want, e.Meta)
	}
	if c != 0 && c != 4 && e.Data != nil {
		t.Fatal("error leaked data")
	}
	return e
}
func queryArgs(e Envelope) []string {
	return []string{"query", e.Meta.RawID, "--server", "example-server", "--tool", "example_read", "--expect", "true", "--jq", ".items[]"}
}
func TestCallFailures(t *testing.T) {
	for _, tc := range []struct {
		mode string
		code int
	}{{"fetch", 2}, {"auth", 3}, {"rpc", 2}, {"isError", 2}, {"disconnect", 2}, {"missing", 2}, {"schema", 5}, {"truncate", 4}, {"pages", 0}, {"cycle", 4}, {"page-auth", 3}, {"sse", 0}, {"text", 0}, {"deadline", 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			a, c := fixture(t, tc.mode)
			e := run(t, a, callArgs(), tc.code)
			if tc.mode == "schema" && *c != 0 {
				t.Fatal("called after schema mismatch")
			}
			if tc.mode == "pages" && len(e.Data) != 2 {
				t.Fatal("page merge")
			}
		})
	}
}
func TestQueryIntegrity(t *testing.T) {
	for _, mode := range []string{"valid", "stale", "hash", "clock", "mismatch", "max-age", "partial", "schema", "permission", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			fm := ""
			if mode == "partial" {
				fm = "truncate"
			}
			a, c := fixture(t, fm)
			code := 0
			if mode == "partial" {
				code = 4
			}
			e := run(t, a, callArgs(), code)
			args := queryArgs(e)
			want := 0
			switch mode {
			case "stale":
				a.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
				want = 6
			case "clock":
				requireNoError(t, os.Chtimes(e.Meta.Raw, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
				want = 6
			case "hash":
				b, _ := os.ReadFile(e.Meta.Raw)
				b = []byte(strings.Replace(string(b), "9007199254740993123456789", "9007199254740993123456788", 1))
				requireNoError(t, os.WriteFile(e.Meta.Raw, b, 0600))
				want = 2
			case "mismatch":
				args[3] = "example-other"
				want = 2
			case "max-age":
				args = append(args, "--max-age", "16m")
				want = 1
			case "partial":
				want = 4
			case "schema":
				b, _ := os.ReadFile(a.ConfigPath)
				var cfg Config
				requireNoError(t, json.Unmarshal(b, &cfg))
				s := cfg.Servers["example-server"]
				p := s.Tools["example_read"]
				p.SchemaHash = strings.Repeat("a", 64)
				s.Tools["example_read"] = p
				b, _ = json.Marshal(cfg)
				requireNoError(t, os.WriteFile(a.ConfigPath, b, 0600))
				want = 5
			case "permission":
				requireNoError(t, os.Chmod(a.ConfigPath, 0666))
				want = 5
			case "symlink":
				p := e.Meta.Raw
				requireNoError(t, os.Rename(p, p+"-other"))
				requireNoError(t, os.Symlink(p+"-other", p))
				want = 2
			}
			before := *c
			run(t, a, args, want)
			if *c != before {
				t.Fatal("query made network call")
			}
		})
	}
}
func TestPreflight(t *testing.T) {
	for _, mode := range []string{"expect", "jq", "denied", "args", "disabled", "raw-id", "unknown-flag"} {
		t.Run(mode, func(t *testing.T) {
			a, c := fixture(t, "")
			args := callArgs()
			want := 1
			switch mode {
			case "expect":
				args = args[:5]
			case "jq":
				args[len(args)-1] = "["
			case "denied":
				args[2] = "delete_example"
				want = 5
			case "args":
				args[4] = `{"limit":3}`
				want = 5
			case "disabled":
				t.Setenv("MCPQ_DISABLE", "1")
				want = 5
			case "raw-id":
				args = []string{"query", "../bad", "--server", "example-server", "--tool", "example_read", "--expect", "true"}
			case "unknown-flag":
				args = append(args, "--oops")
			}
			run(t, a, args, want)
			if *c != 0 {
				t.Fatal("preflight sent call")
			}
		})
	}
}
func TestNumbersAndExpect(t *testing.T) {
	for _, expr := range []string{"false", "null", "true,true", "empty", "error(\"secret\")"} {
		t.Run(expr, func(t *testing.T) { a, _ := fixture(t, ""); args := callArgs(); args[6] = expr; run(t, a, args, 2) })
	}
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	b, _ := json.Marshal(e.Data)
	if string(b) != "[9007199254740993123456789]" {
		t.Fatal(string(b))
	}
	q := queryArgs(e)
	q[len(q)-1] = ".items[0]+1"
	e = run(t, a, q, 0)
	b, _ = json.Marshal(e.Data)
	if string(b) != "[9007199254740993123456790]" {
		t.Fatal(string(b))
	}
	q[len(q)-1] = ".missing"
	run(t, a, q, 2)
	q[len(q)-1] = "empty"
	e = run(t, a, q, 0)
	if e.Meta.Matched != 0 || len(e.Meta.TopKeys) == 0 {
		t.Fatal("zero matches metadata")
	}
}
func TestStorageAndShape(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	st, _ := os.Stat(e.Meta.Raw)
	if st.Mode().Perm() != 0600 {
		t.Fatal("file mode")
	}
	st, _ = os.Stat(filepath.Dir(e.Meta.Raw))
	if st.Mode().Perm() != 0700 {
		t.Fatal("directory mode")
	}
	a.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	run(t, a, queryArgs(e), 2)
	if _, err := os.Stat(e.Meta.Raw); !os.IsNotExist(err) {
		t.Fatal("not expired")
	}
	b, _ := fixture(t, "")
	cfgData, _ := os.ReadFile(b.ConfigPath)
	var cfg Config
	requireNoError(t, json.Unmarshal(cfgData, &cfg))
	requireNoError(t, os.WriteFile(cfg.RawDir, []byte("blocked"), 0600))
	run(t, b, callArgs(), 2)
	c, _ := fixture(t, "shape")
	first := run(t, c, callArgs(), 0)
	var saved map[string]any
	raw, _ := os.ReadFile(first.Meta.Raw)
	requireNoError(t, json.Unmarshal(raw, &saved))
	shapeFiles, _ := filepath.Glob(filepath.Join(filepath.Dir(first.Meta.Raw), "shape-*.json"))
	if len(shapeFiles) != 1 {
		t.Fatal("shape baseline")
	}
	requireNoError(t, os.WriteFile(shapeFiles[0], []byte(`["items","next"]`), 0600))
	e = run(t, c, callArgs(), 2)
	if e.Meta.Shape != "changed" {
		t.Fatal("shape not detected")
	}
}
func TestPartialAndCompare(t *testing.T) {
	a, _ := fixture(t, "truncate")
	args := append(callArgs(), "--allow-partial")
	e := run(t, a, args, 4)
	if e.Data == nil {
		t.Fatal("partial absent")
	}
	q := append(queryArgs(e), "--allow-partial")
	run(t, a, q, 4)
	b, _ := fixture(t, "")
	one := run(t, b, callArgs(), 0)
	two := run(t, b, callArgs(), 0)
	ce := run(t, b, []string{"compare", "example-server", "example_read", "--raw-id", one.Meta.RawID, "--baseline-id", two.Meta.RawID}, 0)
	if ce.Meta.DiffLog == "" {
		t.Fatal("no diff log")
	}
	run(t, b, []string{"tools", "example-server"}, 0)
	args = append(callArgs(), "--max-bytes", "1")
	run(t, b, args, 4)
}

func TestEnvelopeEmptyData(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	q := queryArgs(e)
	q[len(q)-1] = "empty"
	out := run(t, a, q, 0)
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	requireNoError(t, json.Unmarshal(b, &obj))
	if _, ok := obj["data"]; !ok {
		t.Fatal("successful empty stream must include data:[]")
	}
}
func TestStaleExpectPriority(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	a.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	q := queryArgs(e)
	q[7] = "false"
	run(t, a, q, 2)
}
func TestUnknownCompleteness(t *testing.T) {
	a, _ := fixture(t, "")
	b, _ := os.ReadFile(a.ConfigPath)
	var cfg Config
	requireNoError(t, json.Unmarshal(b, &cfg))
	s := cfg.Servers["example-server"]
	p := s.Tools["example_read"]
	p.Page = nil
	s.Tools["example_read"] = p
	b, _ = json.Marshal(cfg)
	requireNoError(t, os.WriteFile(a.ConfigPath, b, 0600))
	e := run(t, a, callArgs(), 4)
	if e.Meta.Complete != "unknown" {
		t.Fatal("completeness")
	}
	run(t, a, queryArgs(e), 4)
}
func TestMetricsAndPermissions(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	if e.Meta.HTTPRequests != 4 || e.Meta.ToolCalls != 1 || e.Meta.ResponseBytes <= 0 {
		t.Fatalf("metrics: %+v", e.Meta)
	}
	q := run(t, a, queryArgs(e), 0)
	if q.Meta.HTTPRequests != 0 || q.Meta.ToolCalls != 0 || q.Meta.SourceToolCalls != 1 || q.Meta.ResponseBytes != 0 {
		t.Fatal("query metrics")
	}
	for _, mode := range []string{"root-mode", "root-link", "config-link", "over-hour", "negative-age", "missing-query-target", "schema-metrics"} {
		t.Run(mode, func(t *testing.T) {
			fm := ""
			if mode == "schema-metrics" {
				fm = "schema"
			}
			a, _ := fixture(t, fm)
			if mode == "missing-query-target" {
				run(t, a, []string{"query", "abc", "--expect", "true"}, 1)
				return
			}
			if mode == "schema-metrics" {
				e := run(t, a, callArgs(), 5)
				if e.Meta.HTTPRequests != 3 || e.Meta.ResponseBytes <= 0 {
					t.Fatal("schema failure metrics lost")
				}
				return
			}
			if mode == "over-hour" {
				b, _ := os.ReadFile(a.ConfigPath)
				var c Config
				requireNoError(t, json.Unmarshal(b, &c))
				c.MaxAge = "61m"
				b, _ = json.Marshal(c)
				requireNoError(t, os.WriteFile(a.ConfigPath, b, 0600))
				run(t, a, callArgs(), 5)
				return
			}
			e := run(t, a, callArgs(), 0)
			switch mode {
			case "root-mode":
				requireNoError(t, os.Chmod(filepath.Dir(e.Meta.Raw), 0755))
				run(t, a, queryArgs(e), 2)
			case "root-link":
				root := filepath.Dir(e.Meta.Raw)
				requireNoError(t, os.Rename(root, root+"-other"))
				requireNoError(t, os.Symlink(root+"-other", root))
				run(t, a, queryArgs(e), 2)
			case "config-link":
				requireNoError(t, os.Rename(a.ConfigPath, a.ConfigPath+"-other"))
				requireNoError(t, os.Symlink(a.ConfigPath+"-other", a.ConfigPath))
				run(t, a, queryArgs(e), 5)
			case "negative-age":
				a.Now = func() time.Time { return e.Meta.FetchedAt.Add(-time.Second) }
				run(t, a, queryArgs(e), 6)
			}
		})
	}
}
func TestExitPriority(t *testing.T) {
	for _, tc := range []struct {
		codes []int
		want  int
	}{{[]int{4, 6, 2, 3, 5, 1}, 1}, {[]int{4, 6, 2, 3, 5}, 5}, {[]int{4, 6, 2, 3}, 3}, {[]int{4, 6, 2}, 2}, {[]int{4, 6}, 6}} {
		errs := []error{}
		for _, n := range tc.codes {
			errs = append(errs, fail(n, "example"))
		}
		if got := errCode(prefer(errs...)); got != tc.want {
			t.Fatalf("%d != %d", got, tc.want)
		}
	}
}

func TestMetadataCorruption(t *testing.T) {
	a, _ := fixture(t, "truncate")
	e := run(t, a, callArgs(), 4)
	b, _ := os.ReadFile(e.Meta.Raw)
	b = []byte(strings.Replace(string(b), `"complete":false`, `"complete":true`, 1))
	requireNoError(t, os.WriteFile(e.Meta.Raw, b, 0600))
	requireNoError(t, os.Chtimes(e.Meta.Raw, e.Meta.FetchedAt, e.Meta.FetchedAt))
	run(t, a, queryArgs(e), 2)
}
func TestInitialCursorDenied(t *testing.T) {
	a, calls := fixture(t, "")
	b, _ := os.ReadFile(a.ConfigPath)
	var cfg Config
	requireNoError(t, json.Unmarshal(b, &cfg))
	s := cfg.Servers["example-server"]
	p := s.Tools["example_read"]
	p.Args["cursor"] = []any{"example-next"}
	s.Tools["example_read"] = p
	b, _ = json.Marshal(cfg)
	requireNoError(t, os.WriteFile(a.ConfigPath, b, 0600))
	args := callArgs()
	args[4] = `{"limit":2,"cursor":"example-next"}`
	run(t, a, args, 5)
	if *calls != 0 {
		t.Fatal("initial cursor sent")
	}
}
func TestNumericBoundaries(t *testing.T) {
	for _, expr := range []string{`[-9007199254740993,9007199254740992,9223372036854775808]|sort|.[2]==9223372036854775808`, `9007199254740993123456789+1==9007199254740993123456790`, `-9007199254740993123456789-1 == -9007199254740993123456790`} {
		t.Run(expr, func(t *testing.T) { a, _ := fixture(t, ""); args := callArgs(); args[6] = expr; run(t, a, args, 0) })
	}
}

func TestCleanupDoesNotDeleteUnrelatedFiles(t *testing.T) {
	a, _ := fixture(t, "")
	b, _ := os.ReadFile(a.ConfigPath)
	var c Config
	requireNoError(t, json.Unmarshal(b, &c))
	requireNoError(t, os.Mkdir(c.RawDir, 0700))
	path := filepath.Join(c.RawDir, "example-unrelated.json")
	requireNoError(t, os.WriteFile(path, []byte(`{"example":true}`), 0600))
	old := time.Now().Add(-25 * time.Hour)
	requireNoError(t, os.Chtimes(path, old, old))
	run(t, a, callArgs(), 2)
	if _, e := os.Stat(path); e != nil {
		t.Fatal("cleanup deleted unrelated file")
	}
}
