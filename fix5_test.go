package main

import (
	"os"
	"path/filepath"
	"testing"
)

func completenessConfig(t *testing.T, a *App, declaration any) {
	t.Helper()
	rewriteConfig(t, a, func(m map[string]any) {
		p := m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)
		delete(p, "page")
		if declaration != nil {
			p["completeness"] = declaration
		}
	})
}

func TestFix5Completeness(t *testing.T) {
	for _, tc := range []struct {
		name, response, mode, items, complete string
		code                                  int
	}{
		{"single", "", "single", "", "true", 0},
		{"below", "", "limit", ".items", "true", 0},
		{"full", "truncate", "limit", ".items", "false", 4},
		{"text", "text", "limit", ".items", "true", 0},
		{"missing", "", "limit", ".missing", "unknown", 4},
		{"not-array", "", "limit", ".next", "unknown", 4},
		{"multiple", "", "limit", ".items, .items", "unknown", 4},
		{"undeclared", "", "", "", "unknown", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := fixture(t, tc.response)
			var declaration any
			if tc.mode != "" {
				declaration = map[string]any{"mode": tc.mode}
			}
			if tc.mode == "limit" {
				declaration = map[string]any{"mode": tc.mode, "items": tc.items, "limit_arg": "limit"}
			}
			completenessConfig(t, a, declaration)
			e := run(t, a, callArgs(), tc.code)
			if e.Meta.Complete != Completeness(tc.complete) || *calls != 1 {
				t.Fatalf("meta=%+v calls=%d", e.Meta, *calls)
			}
			q := run(t, a, queryArgs(e), tc.code)
			if q.Meta.Complete != e.Meta.Complete {
				t.Fatal(q.Meta)
			}
			run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}, tc.code)
		})
	}
}

func TestFix5Config(t *testing.T) {
	for _, name := range []string{"both", "missing-arg", "empty-values", "invalid-value", "unknown-mode", "missing-items", "invalid-items", "single-fields"} {
		t.Run(name, func(t *testing.T) {
			a, calls := fixture(t, "")
			completenessConfig(t, a, map[string]any{"mode": "limit", "items": ".items", "limit_arg": "limit"})
			rewriteConfig(t, a, func(m map[string]any) {
				p := m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)
				d := p["completeness"].(map[string]any)
				switch name {
				case "both":
					p["page"] = map[string]any{"items": "items", "next": "next", "limit_arg": "limit", "cursor_arg": "cursor"}
				case "missing-arg":
					d["limit_arg"] = "absent"
				case "empty-values":
					p["args"] = map[string]any{"limit": []any{}}
				case "invalid-value":
					p["args"] = map[string]any{"limit": []any{2, 0}}
				case "unknown-mode":
					d["mode"] = "other"
				case "missing-items":
					delete(d, "items")
				case "invalid-items":
					d["items"] = ".["
				case "single-fields":
					d["mode"] = "single"
				}
			})
			run(t, a, callArgs(), 5)
			if *calls != 0 {
				t.Fatal("called before validation")
			}
		})
	}
}

func TestFix5Baseline(t *testing.T) {
	for _, mode := range []string{"single", "limit"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := fixture(t, "")
			d := map[string]any{"mode": mode}
			if mode == "limit" {
				d["items"] = ".items"
				d["limit_arg"] = "limit"
			}
			completenessConfig(t, a, d)
			e := run(t, a, callArgs(), 0)
			path := filepath.Join(t.TempDir(), "baseline.json")
			requireNoError(t, os.WriteFile(path, []byte(`{"structuredContent":{"items":[9007199254740993123456789],"next":null}}`), 0600))
			run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 0)
			if mode == "limit" {
				requireNoError(t, os.WriteFile(path, []byte(`{"structuredContent":{"items":[1,2],"next":null}}`), 0600))
				run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 4)
			}
		})
	}
}

func TestFix5BudgetAndExpect(t *testing.T) {
	a, _ := fixture(t, "")
	completenessConfig(t, a, map[string]any{"mode": "single"})
	e := run(t, a, append(callArgs(), "--max-bytes", "2"), 4)
	if e.Meta.Complete != "false" {
		t.Fatal(e.Meta)
	}
	q := run(t, a, queryArgs(e), 4)
	if q.Meta.Complete != "false" {
		t.Fatal(q.Meta)
	}
	args := callArgs()
	args[6] = "false"
	run(t, a, args, 2)
	run(t, a, callArgs()[:5], 1)
}

// Runtime limits come from each call, rather than the first allowlisted value.
func TestFix5ActualLimit(t *testing.T) {
	a, _ := fixture(t, "")
	completenessConfig(t, a, map[string]any{"mode": "limit", "items": ".items", "limit_arg": "limit"})
	rewriteConfig(t, a, func(m map[string]any) {
		p := m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)
		p["args"] = map[string]any{"limit": []any{2, 1}}
		p["required"] = []any{}
	})
	args := callArgs()
	args[4] = `{"limit":1}`
	e := run(t, a, args, 4)
	if e.Meta.Complete != "false" {
		t.Fatal(e.Meta)
	}
	args[4] = `{}`
	e = run(t, a, args, 4)
	if e.Meta.Complete != "unknown" {
		t.Fatal(e.Meta)
	}
	args[4] = `{"limit":3}`
	run(t, a, args, 5)
}

func TestFix5BaselineUnknown(t *testing.T) {
	a, _ := fixture(t, "")
	e := run(t, a, callArgs(), 0)
	// Keep a previously complete snapshot so only the external side can fail.
	completenessConfig(t, a, nil)
	path := filepath.Join(t.TempDir(), "baseline.json")
	requireNoError(t, os.WriteFile(path, []byte(`{"structuredContent":{"items":[9007199254740993123456789],"next":null}}`), 0600))
	run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 4)
	completenessConfig(t, a, map[string]any{"mode": "limit", "items": ".missing", "limit_arg": "limit"})
	run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 4)
}

func TestFix5AcceptedShapeSurvivesLaterFailure(t *testing.T) {
	a, _ := fixture(t, "shape")
	e := run(t, a, callArgs(), 0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(e.Meta.Raw), "shape-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	requireNoError(t, os.WriteFile(files[0], []byte(`["items","next"]`), 0600))
	args := append(callArgs(), "--accept-shape-change")
	args[8] = ".missing"
	run(t, a, args, 2)
	b, err := os.ReadFile(files[0])
	requireNoError(t, err)
	if string(b) != `["extra","items","next"]` {
		t.Fatalf("accepted shape rolled back: %s", b)
	}
	run(t, a, callArgs(), 0)
}
