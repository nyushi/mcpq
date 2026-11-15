package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rewriteConfig(t *testing.T, a *App, change func(map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := decode(b)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	change(m)
	if err := os.WriteFile(a.ConfigPath, mustJSON(m), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFix4HTTP(t *testing.T) {
	a, _ := fixture(t, "")
	rewriteConfig(t, a, func(m map[string]any) {
		m["servers"].(map[string]any)["example-server"].(map[string]any)["allow_plain_http_hosts"] = []any{"bridge.example"}
	})
	c, err := loadConfig(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host    string
		allowed bool
	}{
		{"bridge.example", true}, {"bridge.example.other", false}, {"other.example", false},
	} {
		t.Setenv("EXAMPLE_ENDPOINT", "http://"+tc.host+":8080/mcp")
		cl, err := newClient(c.Servers["example-server"])
		if (err == nil) != tc.allowed {
			t.Fatalf("%s: %v", tc.host, err)
		}
		if cl != nil {
			cl.http.CloseIdleConnections()
		}
	}
	rewriteConfig(t, a, func(m map[string]any) {
		m["servers"].(map[string]any)["example-server"].(map[string]any)["allow_plain_http_hosts"] = []any{"*.example"}
	})
	if _, err := loadConfig(a.ConfigPath); err == nil {
		t.Fatal("wildcard accepted")
	}
}

func baselineArgs(e Envelope, path string, at time.Time) []string {
	return []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID,
		"--baseline-file", path, "--baseline-fetched-at", at.Format(time.RFC3339Nano)}
}
func TestFix4Baseline(t *testing.T) {
	for _, mode := range []string{"structured", "content", "different", "stale", "future", "permissions", "symlink", "invalid", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := fixture(t, "")
			e := run(t, a, callArgs(), 0)
			body := `{"items":[9007199254740993123456789],"next":null}`
			if mode == "different" {
				body = strings.Replace(body, "23456789", "23456788", 1)
			}
			if mode == "incomplete" {
				body = strings.Replace(body, "null", `"more"`, 1)
			}
			result := map[string]any{"structuredContent": json.RawMessage(body)}
			if mode == "content" {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": body}}}
			}
			if mode == "invalid" {
				result = map[string]any{"other": true}
			}
			path := filepath.Join(t.TempDir(), "baseline.json")
			if err := os.WriteFile(path, mustJSON(result), 0600); err != nil {
				t.Fatal(err)
			}
			at := e.Meta.FetchedAt
			want := 0
			switch mode {
			case "different", "invalid":
				want = 2
			case "incomplete":
				want = 4
			case "stale":
				at = at.Add(-time.Hour)
				want = 6
			case "future":
				at = at.Add(time.Hour)
				want = 6
			case "permissions":
				requireNoError(t, os.Chmod(path, 0660))
				want = 2
			case "symlink":
				requireNoError(t, os.Rename(path, path+".target"))
				requireNoError(t, os.Symlink(path+".target", path))
				want = 2
			}
			out := run(t, a, baselineArgs(e, path, at), want)
			if mode == "structured" || mode == "content" || mode == "different" {
				b, err := os.ReadFile(out.Meta.DiffLog)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), "items") || strings.Contains(string(b), "900719") {
					t.Fatal("body in diff log")
				}
			}
			args := append(baselineArgs(e, path, at), "--baseline-id", e.Meta.RawID)
			run(t, a, args, 1)
		})
	}
}

func TestFix4Shape(t *testing.T) {
	a, _ := fixture(t, "shape")
	first := run(t, a, callArgs(), 0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(first.Meta.Raw), "shape-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err := os.WriteFile(files[0], []byte(`["items","next","removed"]`), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, a, callArgs(), 2)
	e := run(t, a, append(callArgs(), "--accept-shape-change"), 0)
	b, _ := json.Marshal(e.Meta)
	var m map[string]any
	requireNoError(t, json.Unmarshal(b, &m))
	if e.Meta.Shape != "changed" || hashValue(m["shape_added"]) != hashValue([]string{"extra"}) || hashValue(m["shape_removed"]) != hashValue([]string{"removed"}) {
		t.Fatalf("%s", b)
	}
	log, err := os.ReadFile(e.Meta.DiffLog)
	if err != nil {
		t.Fatal(err)
	}
	var diff map[string]any
	if json.Unmarshal(log, &diff) != nil {
		t.Fatal("invalid log")
	}
	if hashValue(diff["old_keys"]) != hashValue([]string{"items", "next", "removed"}) || hashValue(diff["new_keys"]) != hashValue([]string{"extra", "items", "next"}) {
		t.Fatalf("%s", log)
	}
	e = run(t, a, callArgs(), 0)
	if e.Meta.Shape != "unchanged" {
		t.Fatal(e.Meta)
	}
	run(t, a, append(queryArgs(e), "--accept-shape-change"), 1)
	run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID, "--accept-shape-change"}, 1)
}
func TestFix4Rejected(t *testing.T) {
	for _, mode := range []string{"expect", "shape", "page", "tool", "jq", "transport", "auth"} {
		t.Run(mode, func(t *testing.T) {
			fm := ""
			if mode == "tool" {
				fm = "isError"
			}
			if mode == "transport" {
				fm = "fetch"
			}
			if mode == "auth" {
				fm = "auth"
			}
			a, _ := fixture(t, fm)
			args := callArgs()
			want := 2
			switch mode {
			case "expect":
				args[6] = "false"
			case "shape":
				e := run(t, a, callArgs(), 0)
				files, _ := filepath.Glob(filepath.Join(filepath.Dir(e.Meta.Raw), "shape-*.json"))
				requireNoError(t, os.WriteFile(files[0], []byte(`["other"]`), 0600))
			case "page":
				rewriteConfig(t, a, func(m map[string]any) {
					m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)["page"].(map[string]any)["next"] = "missing"
				})
			case "jq":
				args[len(args)-1] = ".missing"
			case "auth":
				want = 3
			}
			e := run(t, a, args, want)
			if mode == "transport" || mode == "auth" {
				if e.Meta.Raw != "" {
					t.Fatal("saved without result")
				}
				return
			}
			b, err := os.ReadFile(e.Meta.Raw)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			requireNoError(t, json.Unmarshal(b, &doc))
			if doc["status"] != "rejected" || doc["reason"] != e.Meta.Reason || len(doc["pages"].([]any)) == 0 {
				t.Fatalf("%s", b)
			}
			st, err := os.Stat(e.Meta.Raw)
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("mode", err)
			}
			q := run(t, a, queryArgs(e), 2)
			if q.Meta.Reason != "snapshot_rejected" {
				t.Fatal(q.Meta)
			}
			cmp := run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}, 2)
			if cmp.Meta.Reason != "snapshot_rejected" {
				t.Fatal(cmp.Meta)
			}
			a.Now = func() time.Time { return e.Meta.FetchedAt.Add(24 * time.Hour) }
			run(t, a, queryArgs(e), 2)
			if _, err := os.Stat(e.Meta.Raw); !os.IsNotExist(err) {
				t.Fatal("rejected not expired", err)
			}
		})
	}
}

func TestFix4BaselineFlags(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"structuredContent":{"items":[9007199254740993123456789],"next":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := baselineArgs(e, path, e.Meta.FetchedAt)
	run(t, a, args[:len(args)-2], 1)
	bad := append([]string{}, args...)
	bad[len(bad)-1] = "not-a-time"
	run(t, a, bad, 1)
	run(t, a, append(args, "--max-age", "16m"), 1)
	a.Now = func() time.Time { return e.Meta.FetchedAt.Add(2 * time.Minute) }
	run(t, a, append(args, "--max-age", "1m"), 6)
	run(t, a, append(queryArgs(e), "--baseline-file", path), 1)
}

func TestFix4ShadowPolicy(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	rewriteConfig(t, a, func(m map[string]any) { m["shadow_jq"] = ".items | length" })
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"structuredContent":{"items":[1],"next":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 0)
}

func TestFix4AcceptDoesNotBypassExpect(t *testing.T) {
	a, _ := fixture(t, "shape")
	e := run(t, a, callArgs(), 0)
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(e.Meta.Raw), "shape-*.json"))
	original := []byte(`["items","next"]`)
	if err := os.WriteFile(files[0], original, 0600); err != nil {
		t.Fatal(err)
	}
	args := append(callArgs(), "--accept-shape-change")
	args[6] = "false"
	run(t, a, args, 2)
	b, err := os.ReadFile(files[0])
	if err != nil || string(b) != string(original) {
		t.Fatal("expect failure changed shape", err)
	}
}

func TestFix4RejectedQueryMetrics(t *testing.T) {
	a, _ := fixture(t, "")
	args := callArgs()
	args[6] = "false"
	e := run(t, a, args, 2)
	q := run(t, a, queryArgs(e), 2)
	if q.Meta.ToolCalls != 0 || q.Meta.HTTPRequests != 0 || q.Meta.ResponseBytes != 0 || q.Meta.SourceToolCalls != 1 {
		t.Fatalf("rejected query reports network activity: %+v", q.Meta)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
