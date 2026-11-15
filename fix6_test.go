package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFix6Signal(t *testing.T) {
	for _, tc := range []struct{ name, more, total, items, want string }{
		{"more", "true", "", "", "false"},
		{"done", "false", "", "", "true"},
		{"short", "", "2", ".items", "false"},
		{"equal", "", "1", ".items", "true"},
		{"excess", "", "0", ".items", "unknown"},
		{"string", "", "\"" + "1" + "\"", ".items", "unknown"},
		{"fraction", "", "1.5", ".items", "unknown"},
		{"boolean", "", "true", ".items", "unknown"},
		{"null", "", "null", ".items", "unknown"},
		{"multiple", "", "1,1", ".items", "unknown"},
		{"error", "", "error(\"bad\")", ".items", "unknown"},
		{"missing-total", "", ".absent", ".items", "unknown"},
		{"missing-more", ".absent", "", "", "unknown"},
		{"invalid-more", "1", "", "", "unknown"},
		{"multiple-more", "false,false", "", "", "unknown"},
		{"missing-items", "", "1", ".absent", "unknown"},
		{"nonarray", "", "1", ".next", "unknown"},
		{"both-more", "true", "1", ".items", "false"},
		{"both-short", "false", "2", ".items", "false"},
		{"both-done", "false", "1", ".items", "true"},
		{"both-missing", "false", ".absent", ".items", "unknown"},
		{"more-missing-total", "true", ".absent", ".items", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := fixture(t, "")
			d := map[string]any{"mode": "signal"}
			if tc.more != "" {
				d["has_more"] = tc.more
			}
			if tc.total != "" {
				d["total"] = tc.total
				d["items"] = tc.items
			}
			completenessConfig(t, a, d)
			code := 4
			if tc.want == "true" {
				code = 0
			}
			e := run(t, a, callArgs(), code)
			if e.Meta.Complete != Completeness(tc.want) || *calls != 1 {
				t.Fatalf("meta=%+v calls=%d", e.Meta, *calls)
			}
			q := run(t, a, queryArgs(e), code)
			if q.Meta.Complete != e.Meta.Complete {
				t.Fatal(q.Meta)
			}
			run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}, code)
		})
	}
}

func TestFix6Config(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    map[string]any
	}{
		{"empty", map[string]any{"mode": "signal"}},
		{"total-only", map[string]any{"mode": "signal", "total": ".total"}},
		{"limit-field", map[string]any{"mode": "signal", "has_more": ".more", "limit_arg": "limit"}},
		{"single-field", map[string]any{"mode": "single", "has_more": ".more"}},
		{"limit-signal", map[string]any{"mode": "limit", "items": ".items", "limit_arg": "limit", "total": ".total"}},
		{"bad-path", map[string]any{"mode": "signal", "has_more": ".["}},
		{"page", map[string]any{"mode": "signal", "has_more": ".more"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := fixture(t, "")
			completenessConfig(t, a, tc.d)
			if tc.name == "page" {
				rewriteConfig(t, a, func(m map[string]any) {
					p := m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)
					p["page"] = map[string]any{"items": "items", "next": "next", "limit_arg": "limit", "cursor_arg": "cursor"}
				})
			}
			run(t, a, callArgs(), 5)
			if *calls != 0 {
				t.Fatal("called before validation")
			}
		})
	}
}

func TestFix6BaselineAndBudget(t *testing.T) {
	a, _ := fixture(t, "")
	completenessConfig(t, a, map[string]any{"mode": "signal", "has_more": ".next != null and .next != \"\"", "items": ".items", "total": ".items | length"})
	e := run(t, a, callArgs(), 0)
	path := filepath.Join(t.TempDir(), "baseline.json")
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"structuredContent":{"items":[9007199254740993123456789],"next":null}}`, 0},
		{`{"structuredContent":{"items":[9007199254740993123456789],"next":"later"}}`, 4},
	} {
		requireNoError(t, os.WriteFile(path, []byte(tc.body), 0600))
		run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), tc.code)
	}
	e = run(t, a, append(callArgs(), "--max-bytes", "2"), 4)
	if e.Meta.Complete != "false" {
		t.Fatal(e.Meta)
	}
	q := run(t, a, queryArgs(e), 4)
	if q.Meta.Complete != "false" {
		t.Fatal(q.Meta)
	}
}

func TestFix6TargetAndSavedCompleteness(t *testing.T) {
	for _, response := range []string{"", "text", "truncate"} {
		t.Run(response, func(t *testing.T) {
			a, _ := fixture(t, response)
			completenessConfig(t, a, map[string]any{"mode": "signal", "has_more": ".next != null and .next != \"\"", "items": ".items", "total": "1"})
			code := 0
			want := Completeness("true")
			if response == "text" {
				code = 4
				want = "false"
			}
			if response == "truncate" {
				code = 4
				want = "unknown"
			}
			e := run(t, a, callArgs(), code)
			if e.Meta.Complete != want {
				t.Fatal(e.Meta)
			}
			completenessConfig(t, a, map[string]any{"mode": "single"})
			q := run(t, a, queryArgs(e), code)
			if q.Meta.Complete != want {
				t.Fatal(q.Meta)
			}
			run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}, code)
		})
	}
}
