package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/itchyny/gojq"
)

func compile(expr string) (*gojq.Code, error) {
	q, e := gojq.Parse(expr)
	if e != nil {
		return nil, fail(1, "jq_syntax")
	}
	c, e := gojq.Compile(q)
	if e != nil {
		return nil, fail(1, "jq_compile")
	}
	return c, nil
}
func evaluate(ctx context.Context, c *gojq.Code, v any) ([]any, error) {
	it := c.RunWithContext(ctx, jqNumbers(v))
	out := []any{}
	for {
		x, ok := it.Next()
		if !ok {
			break
		}
		if _, ok := x.(error); ok {
			return nil, fail(2, "jq_evaluation")
		}
		out = append(out, x)
		if len(out) > 100000 {
			return nil, fail(2, "jq_result_limit")
		}
	}
	if ctx.Err() != nil {
		return nil, fail(2, "deadline")
	}
	return out, nil
}
func expectOK(ctx context.Context, c *gojq.Code, v any) error {
	r, e := evaluate(ctx, c, v)
	if e != nil {
		return e
	}
	if len(r) != 1 || r[0] != true {
		return fail(2, "expect_failed")
	}
	return nil
}
func target(page any) (any, int, error) {
	m, ok := page.(map[string]any)
	if !ok {
		return nil, 0, fail(2, "result_shape")
	}
	if e, ok := m["isError"]; ok {
		b, ok := e.(bool)
		if !ok || b {
			return nil, 0, fail(2, "tool_error")
		}
	}
	blocks, has := m["content"]
	content, _ := blocks.([]any)
	if has && content == nil {
		return nil, 0, fail(2, "content_shape")
	}
	if v, ok := m["structuredContent"]; ok {
		return v, len(content), nil
	}
	texts := []any{}
	parsed := []any{}
	allJSON := true
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			return nil, len(content), fail(2, "block_shape")
		}
		if b["type"] != "text" {
			continue
		}
		s, ok := b["text"].(string)
		if !ok {
			return nil, len(content), fail(2, "text_shape")
		}
		texts = append(texts, s)
		v, e := decode([]byte(s))
		if e != nil {
			allJSON = false
		}
		parsed = append(parsed, v)
	}
	if !allJSON {
		return texts, len(content), nil
	}
	if len(parsed) == 1 {
		return parsed[0], len(content), nil
	}
	return parsed, len(content), nil
}
func getPath(v any, path string) (any, bool) {
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return v, true
}
func putPath(v any, path string, value any) error {
	parts := strings.Split(path, ".")
	for _, k := range parts[:len(parts)-1] {
		m, ok := v.(map[string]any)
		if !ok {
			return fail(2, "page_shape")
		}
		v = m[k]
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fail(2, "page_shape")
	}
	m[parts[len(parts)-1]] = value
	return nil
}
func merged(pages []any, p Policy) (any, error) {
	targets := []any{}
	items := []any{}
	for _, page := range pages {
		v, _, e := target(page)
		if e != nil {
			return nil, e
		}
		targets = append(targets, v)
		if p.Page != nil {
			x, ok := getPath(v, p.Page.Items)
			arr, yes := x.([]any)
			if !ok || !yes {
				return nil, fail(2, "page_items")
			}
			items = append(items, arr...)
		}
	}
	if len(targets) == 0 {
		return nil, fail(2, "no_pages")
	}
	v := targets[len(targets)-1]
	if p.Page != nil {
		b, _ := json.Marshal(v)
		v, _ = decode(b)
		if e := putPath(v, p.Page.Items, items); e != nil {
			return nil, e
		}
	}
	return v, nil
}
