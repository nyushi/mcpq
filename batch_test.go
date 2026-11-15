package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchVerdicts(t *testing.T) {
	for _, tc := range []struct {
		alert   string
		code    int
		verdict string
	}{
		{"empty", 0, "clear"}, {"{value: .items[0]}", 7, "alert"}, {"null", 2, "error"}, {"$vars.missing", 2, "error"},
	} {
		t.Run(tc.alert, func(t *testing.T) {
			a, calls := fixture(t, "")
			path := filepath.Join(t.TempDir(), "spec.json")
			spec := fmt.Sprintf(`{"steps":[{"id":"first","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":%q},{"id":"second","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":"empty"}]}`, tc.alert)
			if err := os.WriteFile(path, []byte(spec), 0600); err != nil {
				t.Fatal(err)
			}
			env, code := a.Run([]string{"batch", path})
			if code != tc.code {
				t.Fatalf("code=%d want=%d meta=%+v", code, tc.code, env.Meta)
			}
			if env.Meta.HTTPRequests != 5 {
				t.Fatalf("requests=%d", env.Meta.HTTPRequests)
			}
			if *calls != 2 {
				t.Fatalf("calls=%d", *calls)
			}
			b := string(mustJSON(env))
			if !containsBatch(b, `"verdict":"`+tc.verdict+`"`) {
				t.Fatal(b)
			}
		})
	}
}
func containsBatch(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBatchValidationAndContinuation(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		code, calls int
	}{
		{"unknown", `{"extra":1,"steps":[]}`, 1, 0},
		{"empty", `{"steps":[]}`, 1, 0},
		{"denied", `{"steps":[{"id":"a","server":"example-server","tool":"example_read","args":{"limit":3},"expect":"true","alert":"empty"}]}`, 5, 0},
		{"continue", `{"steps":[{"id":"a","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"false","alert":"empty"},{"id":"b","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":"{}"}]}`, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := fixture(t, "")
			p := filepath.Join(t.TempDir(), "spec.json")
			if e := os.WriteFile(p, []byte(tc.body), 0600); e != nil {
				t.Fatal(e)
			}
			env, code := a.Run([]string{"batch", p})
			if code != tc.code || *calls != tc.calls {
				t.Fatalf("code %d calls %d meta %+v", code, *calls, env.Meta)
			}
			if code != 0 && env.Data != nil {
				t.Fatal("failure exposed data")
			}
		})
	}
}
func TestBatchPartialAndPermissions(t *testing.T) {
	a, _ := fixture(t, "truncate")
	p := filepath.Join(t.TempDir(), "spec.json")
	body := `{"vars":{"threshold":1},"steps":[{"id":"a","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":"{value:$vars.threshold}"}]}`
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	env, code := a.Run([]string{"batch", p})
	if code != 4 || env.Data != nil {
		t.Fatal(code, env)
	}
	env, code = a.Run([]string{"batch", p, "--allow-partial"})
	if code != 4 || len(env.Data) != 1 {
		t.Fatal(code, env)
	}
	if env.Meta.HTTPRequests != 4 || env.Meta.ToolCalls != 1 {
		t.Fatal(env.Meta)
	}
	hash := env.Meta.SpecSHA256
	if e := os.WriteFile(p, []byte(body+" "), 0600); e != nil {
		t.Fatal(e)
	}
	env, _ = a.Run([]string{"batch", p})
	if hash == env.Meta.SpecSHA256 {
		t.Fatal("unchanged hash")
	}
	if e := os.Chmod(p, 0666); e != nil {
		t.Fatal(e)
	}
	_, code = a.Run([]string{"batch", p})
	if code != 5 {
		t.Fatal(code)
	}
	link := p + "-link"
	if e := os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	_, code = a.Run([]string{"batch", link})
	if code != 5 {
		t.Fatal(code)
	}
}

func TestBatchSpecBoundaries(t *testing.T) {
	step := `{"id":"a","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":"empty"}`
	for _, body := range []string{`{"steps":[` + step + `,` + step + `]}`, `{"steps":[` + strings.Replace(step, `"alert":`, `"typo":0,"alert":`, 1) + `]}`, `{"steps":[` + strings.TrimSuffix(strings.Repeat(step+",", 21), ",") + `]}`} {
		a, calls := fixture(t, "")
		p := filepath.Join(t.TempDir(), "spec.json")
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		_, code := a.Run([]string{"batch", p})
		if code != 1 || *calls != 0 {
			t.Fatal(code, *calls)
		}
	}
}

func TestBatchUnknownCompletenessAndAuthentication(t *testing.T) {
	for _, mode := range []string{"unknown", "auth"} {
		t.Run(mode, func(t *testing.T) {
			a, calls := fixture(t, mode)
			if mode == "unknown" {
				c, e := loadConfig(a.ConfigPath)
				if e != nil {
					t.Fatal(e)
				}
				server := c.Servers["example-server"]
				p := server.Tools["example_read"]
				p.Page = nil
				server.Tools["example_read"] = p
				if e = os.WriteFile(a.ConfigPath, mustJSON(c), 0600); e != nil {
					t.Fatal(e)
				}
			}
			p := filepath.Join(t.TempDir(), "spec.json")
			step := `{"id":"a","server":"example-server","tool":"example_read","args":{"limit":2},"expect":"true","alert":"{}"}`
			body := `{"steps":[` + step + `,` + strings.Replace(step, `"id":"a"`, `"id":"b"`, 1) + `]}`
			if e := os.WriteFile(p, []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			env, code := a.Run([]string{"batch", p})
			want := 4
			if mode == "auth" {
				want = 3
			}
			if code != want || *calls != 2 || env.Meta.Verdict != "error" || env.Data != nil {
				t.Fatal(code, *calls, env)
			}
			if mode == "unknown" && env.Meta.Steps[0].Complete != "unknown" {
				t.Fatal(env)
			}
		})
	}
}
func TestBatchVariables(t *testing.T) {
	for _, tc := range []struct {
		expr string
		code int
	}{
		{`{value:$vars.threshold}`, 7},
		{`{value:$vars["threshold"]}`, 7},
		{`{value:$vars["absent"]}`, 2},
		{`{value:"$vars.absent"}`, 7},
		{`{value:$vars.nested.value}`, 7},
		{`{value:$vars.nested.absent}`, 2},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			a, _ := fixture(t, "")
			p := filepath.Join(t.TempDir(), "spec.json")
			body := map[string]any{"vars": map[string]any{"threshold": 100, "nested": map[string]any{"value": 2}}, "steps": []any{map[string]any{"id": "a", "server": "example-server", "tool": "example_read", "args": map[string]any{"limit": 2}, "expect": "true", "alert": tc.expr}}}
			if e := os.WriteFile(p, mustJSON(body), 0600); e != nil {
				t.Fatal(e)
			}
			env, code := a.Run([]string{"batch", p})
			if code != tc.code {
				t.Fatal(code, env)
			}
			if tc.code == 7 {
				var decoded map[string]any
				if e := json.Unmarshal(mustJSON(env), &decoded); e != nil {
					t.Fatal(e)
				}
				if decoded["meta"].(map[string]any)["spec"] != "" {
					t.Fatal(decoded)
				}
			}
		})
	}
}
