package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func shapeConfig(t *testing.T, a *App, declaration any) {
	t.Helper()
	rewriteConfig(t, a, func(m map[string]any) {
		p := m["servers"].(map[string]any)["example-server"].(map[string]any)["tools"].(map[string]any)["example_read"].(map[string]any)
		delete(p, "page")
		p["completeness"] = map[string]any{"mode": "single"}
		p["shape"] = declaration
	})
}
func TestFix7Declared(t *testing.T) {
	a, _ := fixture(t, "declared-alternating")
	shapeConfig(t, a, map[string]any{"required": []string{"items"}, "optional": []string{"next"}})
	e := run(t, a, callArgs(), 0)
	if e.Meta.Shape != "declared" {
		t.Fatal(e.Meta)
	}
	c, err := loadConfig(a.ConfigPath)
	requireNoError(t, err)
	path := filepath.Join(c.RawDir, "shape-"+hashValue([]string{"example-server", "example_read"})+".json")
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fingerprint created: %v", err)
	}
	requireNoError(t, os.WriteFile(path, []byte("broken"), 0600))
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	for i := 0; i < 6; i++ {
		body := `{"structuredContent":{"items":[9007199254740993123456789]}}`
		if i%2 == 0 {
			body = `{"structuredContent":{"items":[9007199254740993123456789],"next":null}}`
		}
		requireNoError(t, os.WriteFile(baseline, []byte(body), 0600))
		run(t, a, callArgs(), 0)
		run(t, a, queryArgs(e), 0)
		rewriteConfig(t, a, func(m map[string]any) { m["shadow_jq"] = ".items" })
		run(t, a, baselineArgs(e, baseline, e.Meta.FetchedAt), 0)
	}
	b, err := os.ReadFile(path)
	requireNoError(t, err)
	if string(b) != "broken" {
		t.Fatal(string(b))
	}
	run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}, 0)
	for _, tc := range []struct {
		decl           any
		removed, added string
	}{
		{map[string]any{"required": []string{"sprigs"}, "optional": []string{"items", "next"}}, "sprigs", ""},
		{map[string]any{"required": []string{"items"}}, "", "next"},
	} {
		shapeConfig(t, a, tc.decl)
		for _, args := range [][]string{callArgs(), queryArgs(e), baselineArgs(e, baseline, e.Meta.FetchedAt), {"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", e.Meta.RawID}} {
			got := run(t, a, args, 2)
			if got.Meta.Reason != "shape_changed" {
				t.Fatal(got.Meta)
			}
			if tc.removed != "" && (got.Meta.ShapeRemoved == nil || hashValue(*got.Meta.ShapeRemoved) != hashValue([]string{tc.removed})) {
				t.Fatal(got.Meta)
			}
			if tc.added != "" && (got.Meta.ShapeAdded == nil || hashValue(*got.Meta.ShapeAdded) != hashValue([]string{tc.added})) {
				t.Fatal(got.Meta)
			}
		}
	}
}
func TestFix7ConfigAndAccept(t *testing.T) {
	for _, decl := range []any{
		map[string]any{"required": []string{"sprigs", "sprigs"}},
		map[string]any{"optional": []string{"sprigs", "sprigs"}},
		map[string]any{"required": []string{"sprigs"}, "optional": []string{"sprigs"}},
		map[string]any{"required": []any{1}}, map[string]any{"optional": "sprigs"},
		map[string]any{"required": []any{nil}},
	} {
		a, calls := fixture(t, "")
		shapeConfig(t, a, decl)
		run(t, a, callArgs(), 5)
		if *calls != 0 {
			t.Fatal("network before validation")
		}
	}
	a, calls := fixture(t, "")
	shapeConfig(t, a, map[string]any{"optional": []string{"items", "next"}})
	run(t, a, callArgs(), 0)
	before := *calls
	t.Setenv("MCPQ_DISABLE", "1")
	run(t, a, append(callArgs(), "--accept-shape-change"), 1)
	if *calls != before {
		t.Fatal("network before flag rejection")
	}
}

func TestFix7EmptyAndNonObject(t *testing.T) {
	for _, tc := range []struct {
		declaration string
		value       any
		code        int
	}{
		{`{}`, []any{1}, 0}, {`{}`, map[string]any{"sprigs": 1}, 2},
		{`{"required":["sprigs"]}`, []any{1}, 2},
		{`{"optional":["sprigs"]}`, []any{1}, 0},
	} {
		a, _ := fixture(t, "")
		v, err := decode([]byte(tc.declaration))
		requireNoError(t, err)
		shapeConfig(t, a, v)
		c, err := loadConfig(a.ConfigPath)
		requireNoError(t, err)
		m := Meta{Server: "example-server", Tool: "example_read"}
		err = shape(context.Background(), c, &m, tc.value, false)
		code := 0
		if err != nil {
			code = errCode(err)
		}
		if code != tc.code || m.Shape != "declared" {
			t.Fatalf("code=%d meta=%+v", code, m)
		}
	}
}
func TestFix7BaselineViolation(t *testing.T) {
	a, _ := fixture(t, "")
	shapeConfig(t, a, map[string]any{"required": []string{"items"}, "optional": []string{"next"}})
	e := run(t, a, callArgs(), 0)
	path := filepath.Join(t.TempDir(), "baseline.json")
	for _, body := range []string{`{"structuredContent":{"next":null}}`, `{"structuredContent":{"items":[],"brambles":true}}`} {
		requireNoError(t, os.WriteFile(path, []byte(body), 0600))
		got := run(t, a, baselineArgs(e, path, e.Meta.FetchedAt), 2)
		if got.Meta.Reason != "shape_changed" {
			t.Fatal(got.Meta)
		}
	}
	// A previously valid saved baseline must be checked under the current declaration.
	c, err := loadConfig(a.ConfigPath)
	requireNoError(t, err)
	snap, _, err := readSnapshot(c, e.Meta.RawID)
	requireNoError(t, err)
	snap.Pages = []any{map[string]any{"structuredContent": map[string]any{"items": []any{}, "brambles": true}}}
	snap.Integrity = ""
	requireNoError(t, a.save(context.Background(), c, &snap))
	got := run(t, a, []string{"compare", "example-server", "example_read", "--raw-id", e.Meta.RawID, "--baseline-id", snap.Meta.RawID}, 2)
	if got.Meta.Reason != "shape_changed" {
		t.Fatal(got.Meta)
	}
}
