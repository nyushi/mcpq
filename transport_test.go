package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{{"version", 2}, {"capability", 2}, {"notification", 2}, {"list-cycle", 2}, {"list-duplicate", 5}, {"list-missing", 5}, {"list-pages", 0}, {"wrong-id", 2}, {"content-type", 2}, {"redirect", 2}, {"forbidden", 3}, {"oversize", 2}, {"sse-multiline", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := fixture(t, "")
			listCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				json.NewDecoder(r.Body).Decode(&req)
				method := req["method"]
				id := req["id"]
				w.Header().Set("Content-Type", "application/json")
				var result any
				switch method {
				case "initialize":
					version := protocolVersion
					if tc.name == "version" {
						version = "example-unsupported"
					}
					caps := map[string]any{"tools": map[string]any{}}
					if tc.name == "capability" {
						caps = map[string]any{}
					}
					result = map[string]any{"protocolVersion": version, "capabilities": caps}
				case "notifications/initialized":
					if tc.name == "notification" {
						w.WriteHeader(200)
					} else {
						w.WriteHeader(202)
					}
					return
				case "tools/list":
					listCalls++
					tools := []any{map[string]any{"name": "example_read", "inputSchema": map[string]any{"type": "object"}}}
					m := map[string]any{"tools": tools}
					switch tc.name {
					case "list-cycle":
						m["nextCursor"] = "example-loop"
						m["tools"] = []any{}
					case "list-duplicate":
						m["tools"] = append(tools, tools[0])
					case "list-missing":
						m["tools"] = []any{}
					case "list-pages":
						if listCalls == 1 {
							m["tools"] = []any{}
							m["nextCursor"] = "example-next"
						}
					}
					result = m
				case "tools/call":
					result = map[string]any{"structuredContent": map[string]any{"items": []any{1}, "next": nil}}
					switch tc.name {
					case "wrong-id":
						id = 999
					case "content-type":
						w.Header().Set("Content-Type", "text/plain")
					case "redirect":
						w.Header().Set("Location", "/example-redirect")
						w.WriteHeader(307)
						return
					case "forbidden":
						w.WriteHeader(403)
						return
					case "oversize":
						fmt.Fprint(w, strings.Repeat(" ", receiveLimit+1))
						return
					}
				}
				if tc.name == "sse-multiline" {
					b, _ := json.Marshal(result)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":%v,\"result\":%s}\n\n", id, b)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
			}))
			defer srv.Close()
			t.Setenv("EXAMPLE_ENDPOINT", srv.URL)
			run(t, a, callArgs(), tc.want)
		})
	}
}
func TestContentSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		page   string
		want   string
		blocks int
	}{{"structured", `{"structuredContent":{"x":1},"content":[{"type":"text","text":"other"}]}`, `{"x":1}`, 1}, {"structured-null", `{"structuredContent":null}`, `null`, 0}, {"json-blocks", `{"content":[{"type":"text","text":"1"},{"type":"text","text":"2"}]}`, `[1,2]`, 2}, {"text-blocks", `{"content":[{"type":"text","text":"plain"},{"type":"text","text":"2"},{"type":"image","data":"example"}]}`, `["plain","2"]`, 3}, {"empty", `{"content":[]}`, `[]`, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := decode([]byte(tc.page))
			if e != nil {
				t.Fatal(e)
			}
			v, n, e := target(p)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(v)
			if string(b) != tc.want || n != tc.blocks {
				t.Fatalf("%s %d", b, n)
			}
		})
	}
}
func TestJQDeadline(t *testing.T) {
	a, _ := fixture(t, "")
	args := callArgs()
	args[len(args)-1] = "def f: f; f"
	start := time.Now()
	run(t, a, args, 2)
	if time.Since(start) > 2*time.Second {
		t.Fatal("jq exceeded deadline")
	}
}
func TestCompareDifferenceAndRetention(t *testing.T) {
	a, _ := fixture(t, "")
	one := run(t, a, callArgs(), 0)
	two := run(t, a, callArgs(), 0)
	b, _ := os.ReadFile(a.ConfigPath)
	var cfg Config
	json.Unmarshal(b, &cfg)
	s, _, e := readSnapshot(cfg, two.Meta.RawID)
	if e != nil {
		t.Fatal(e)
	}
	s.Pages[0].(map[string]any)["structuredContent"].(map[string]any)["items"] = []any{json.Number("1")}
	s.Meta.RawSHA256 = hashValue(s.Pages)
	s.Integrity = snapshotIntegrity(s)
	b, _ = json.Marshal(s)
	os.WriteFile(s.Meta.Raw, b, 0600)
	os.Chtimes(s.Meta.Raw, s.Meta.FetchedAt, s.Meta.FetchedAt)
	out := run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", one.Meta.RawID, "--baseline-id", two.Meta.RawID}, 2)
	log, st, e := readSafe(out.Meta.DiffLog, receiveLimit)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal("log permissions")
	}
	if strings.Contains(string(log), "9007199254740993123456789") || strings.Contains(string(log), "example-secret") {
		t.Fatal("log leaked content")
	}
	a.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	run(t, a, queryArgs(one), 2)
	if _, e := os.Stat(out.Meta.DiffLog); !os.IsNotExist(e) {
		t.Fatal("log not expired")
	}
}
func TestDeadlineNoProgressReset(t *testing.T) {
	a, _ := fixture(t, "")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				fmt.Fprint(w, ": progress\n\n")
				f.Flush()
			}
		}
	}))
	defer s.Close()
	t.Setenv("EXAMPLE_ENDPOINT", s.URL)
	start := time.Now()
	run(t, a, callArgs(), 2)
	if time.Since(start) > 2*time.Second {
		t.Fatal("progress reset deadline")
	}
}
func TestTokenReread(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.Header.Get("Authorization") != "Bearer example-first" {
				t.Error("first token")
			}
		} else if r.Header.Get("Authorization") != "Bearer example-second" {
			t.Error("token not reread")
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	t.Setenv("EXAMPLE_ENDPOINT", srv.URL)
	t.Setenv("EXAMPLE_TOKEN", "example-first")
	c, e := newClient(Server{EndpointEnv: "EXAMPLE_ENDPOINT", TokenEnv: "EXAMPLE_TOKEN"})
	if e != nil {
		t.Fatal(e)
	}
	defer c.http.CloseIdleConnections()
	c.rpc(context.Background(), "notifications/initialized", nil)
	t.Setenv("EXAMPLE_TOKEN", "example-second")
	c.rpc(context.Background(), "notifications/initialized", nil)
}
