package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/itchyny/gojq"
)

type options struct {
	command, server, tool, id, baseline string
	args                                map[string]any
	jq, expect                          *gojq.Code
	allow                               bool
	maxBytes                            int
	maxAge                              time.Duration
	ageExplicit                         bool
	baselineFile                        string
	baselineAt                          time.Time
	acceptShape                         bool
}

func parse(args []string) (options, error) {
	o := options{args: map[string]any{}, maxBytes: outputLimit, maxAge: 15 * time.Minute}
	if len(args) == 0 {
		return o, fail(1, "command_required")
	}
	o.command = args[0]
	n := 0
	switch o.command {
	case "call", "compare":
		n = 3
	case "query", "tools":
		n = 2
	default:
		return o, fail(1, "unknown_command")
	}
	if len(args) < n {
		return o, fail(1, "positional_required")
	}
	if o.command == "query" {
		o.id = args[1]
		if !validID.MatchString(o.id) {
			return o, fail(1, "raw_id_invalid")
		}
	} else {
		o.server = args[1]
	}
	if n == 3 {
		o.tool = args[2]
	}
	expect := ""
	jq := "."
	seen := map[string]bool{}
	for i := n; i < len(args); i++ {
		k := args[i]
		if seen[k] {
			return o, fail(1, "duplicate_flag")
		}
		seen[k] = true
		if k == "--accept-shape-change" {
			if o.command != "call" {
				return o, fail(1, "flag_not_supported")
			}
			o.acceptShape = true
			continue
		}
		if k == "--allow-partial" {
			if o.command != "call" && o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			o.allow = true
			continue
		}
		if i+1 >= len(args) {
			return o, fail(1, "flag_value_required")
		}
		i++
		v := args[i]
		switch k {
		case "--args":
			if o.command != "call" {
				return o, fail(1, "flag_not_supported")
			}
			x, e := decode([]byte(v))
			if e != nil {
				return o, fail(1, "arguments_json")
			}
			m, ok := x.(map[string]any)
			if !ok {
				return o, fail(1, "arguments_object")
			}
			o.args = m
		case "--expect":
			if o.command != "call" && o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			expect = v
		case "--jq":
			if o.command != "call" && o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			jq = v
		case "--server":
			if o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			o.server = v
		case "--tool":
			if o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			o.tool = v
		case "--max-age":
			if o.command != "query" && o.command != "compare" {
				return o, fail(1, "flag_not_supported")
			}
			d, e := time.ParseDuration(v)
			if e != nil || d <= 0 || d > time.Hour {
				return o, fail(1, "max_age_invalid")
			}
			o.maxAge = d
			o.ageExplicit = true
		case "--max-bytes":
			if o.command != "call" && o.command != "query" {
				return o, fail(1, "flag_not_supported")
			}
			b, e := strconv.Atoi(v)
			if e != nil || b <= 0 || b > outputLimit {
				return o, fail(1, "max_bytes_invalid")
			}
			o.maxBytes = b
		case "--raw-id":
			if o.command != "compare" || !validID.MatchString(v) {
				return o, fail(1, "raw_id_invalid")
			}
			o.id = v
		case "--baseline-file":
			if o.command != "compare" || v == "" {
				return o, fail(1, "baseline_file_invalid")
			}
			o.baselineFile = v
		case "--baseline-fetched-at":
			if o.command != "compare" {
				return o, fail(1, "flag_not_supported")
			}
			at, err := time.Parse(time.RFC3339Nano, v)
			if err != nil || at.IsZero() {
				return o, fail(1, "baseline_time_invalid")
			}
			o.baselineAt = at
		case "--baseline-id":
			if o.command != "compare" || !validID.MatchString(v) {
				return o, fail(1, "baseline_id_invalid")
			}
			o.baseline = v
		default:
			return o, fail(1, "unknown_flag")
		}
	}
	if o.server == "" || (o.command != "tools" && o.tool == "") {
		return o, fail(1, "target_required")
	}
	if o.command == "compare" {
		if o.id == "" || (o.baseline == "") == (o.baselineFile == "") {
			return o, fail(1, "compare_baseline_required")
		}
		if (o.baselineFile != "") != !o.baselineAt.IsZero() {
			return o, fail(1, "baseline_time_required")
		}
	}
	if o.command == "call" || o.command == "query" {
		if expect == "" {
			return o, fail(1, "expect_required")
		}
		var e error
		o.expect, e = compile(expect)
		if e != nil {
			return o, e
		}
		o.jq, e = compile(jq)
		if e != nil {
			return o, e
		}
	}
	return o, nil
}
func (a *App) Run(args []string) (env Envelope, code int) {
	defer func() {
		if recover() != nil {
			env.Data = nil
			env.Meta.Code = 2
			env.Meta.Reason = "unexpected_failure"
			code = 2
		}
	}()
	o, e := parse(args)
	env.Meta = Meta{Server: o.server, Tool: o.tool, Args: o.args}
	finish := func(err error) (Envelope, int) {
		c := 0
		if err != nil {
			c = errCode(err)
			env.Meta.Reason = brief(err)
		}
		env.Meta.Code = c
		if c != 0 && (c != 4 || !o.allow) {
			env.Data = nil
		}
		return env, c
	}
	if e != nil {
		return finish(e)
	}
	path := a.ConfigPath
	if path == "" {
		path = defaultConfigPath
	}
	c, e := loadConfig(path)
	if e != nil {
		return finish(e)
	}
	if o.acceptShape && c.Servers[o.server].Tools[o.tool].Shape != nil {
		return finish(fail(1, "declared_shape_accept"))
	}
	if o.command == "query" || o.command == "compare" {
		upper := duration(c.MaxAge)
		if !o.ageExplicit {
			o.maxAge = upper
			if o.command == "query" && o.maxAge > 15*time.Minute {
				o.maxAge = 15 * time.Minute
			}
		}
		if o.maxAge > upper {
			return finish(fail(1, "max_age_policy"))
		}
	}
	if os.Getenv("MCPQ_DISABLE") == "1" {
		return finish(fail(5, "disabled"))
	}
	s, exists := c.Servers[o.server]
	if o.command != "query" && !exists {
		return finish(fail(5, "server_denied"))
	}
	var p Policy
	switch o.command {
	case "call":
		s, p, e = authorize(c, o.server, o.tool, o.args, false)
		if e != nil {
			return finish(e)
		}
	case "compare":
		var ok bool
		p, ok = s.Tools[o.tool]
		if !ok || dangerous(o.tool) {
			return finish(fail(5, "tool_denied"))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration(c.Deadline))
	defer cancel()
	if e := prepareStore(c.RawDir, a.now()); e != nil {
		return finish(e)
	}
	switch o.command {
	case "call", "tools":
		cl, e := newClient(s)
		if e != nil {
			return finish(e)
		}
		defer cl.http.CloseIdleConnections()
		defer func() {
			env.Meta.HTTPRequests = cl.requests
			env.Meta.ToolCalls = cl.calls
			env.Meta.ResponseBytes = cl.bytes
		}()
		if e = cl.init(ctx); e != nil {
			env.Meta.HTTPRequests = cl.requests
			env.Meta.ResponseBytes = cl.bytes
			return finish(e)
		}
		ts, e := cl.tools(ctx)
		if e != nil {
			env.Meta.HTTPRequests = cl.requests
			env.Meta.ResponseBytes = cl.bytes
			return finish(e)
		}
		if o.command == "tools" {
			names := []string{}
			for n := range s.Tools {
				names = append(names, n)
			}
			sort.Strings(names)
			env.Data = []any{}
			for _, n := range names {
				t, ok := ts[n]
				if !ok || hashValue(t) != s.Tools[n].SchemaHash {
					return finish(fail(5, "schema_changed"))
				}
				env.Data = append(env.Data, t)
			}
			env.Meta.HTTPRequests = cl.requests
			env.Meta.ResponseBytes = cl.bytes
			return finish(nil)
		}
		t, ok := ts[o.tool]
		if !ok || hashValue(t) != p.SchemaHash {
			return finish(fail(5, "schema_changed"))
		}
		snapshot, data, e := a.fetch(ctx, c, cl, o, p)
		env.Meta = snapshot.Meta
		if e != nil {
			return finish(e)
		}
		env.Data = data
		return finish(partial(env.Meta))
	case "query":
		snap, e := a.checkSnapshot(ctx, c, o.id, o.server, o.tool, o.maxAge)
		if snap.Meta.RawID != "" {
			env.Meta = snap.Meta
		}
		freshness := e
		if e != nil && errCode(e) != 6 {
			return finish(e)
		}
		_, p, e = authorize(c, o.server, o.tool, snap.Meta.Args, false)
		if e != nil {
			return finish(e)
		}
		v, e := merged(snap.Pages, p)
		if e != nil {
			return finish(e)
		}
		if e = shape(ctx, c, &env.Meta, v, false); e != nil {
			return finish(e)
		}
		env.Data, e = filter(ctx, o, p, snap.Pages, &env.Meta)
		if e != nil {
			return finish(e)
		}
		return finish(prefer(freshness, partial(env.Meta)))
	case "compare":
		left, le := a.checkSnapshot(ctx, c, o.id, o.server, o.tool, o.maxAge)
		var right Snapshot
		var re error
		if o.baselineFile != "" {
			right, re = a.readBaseline(ctx, o, p, left.Meta)
		} else {
			right, re = a.checkSnapshot(ctx, c, o.baseline, o.server, o.tool, o.maxAge)
		}
		env.Meta = left.Meta
		if le != nil || re != nil {
			chosen := prefer(le, re)
			if re != nil && brief(chosen) == "shape_changed" && brief(re) == "shape_changed" && (le == nil || brief(le) != "shape_changed") {
				env.Meta.Shape, env.Meta.ShapeAdded, env.Meta.ShapeRemoved = right.Meta.Shape, right.Meta.ShapeAdded, right.Meta.ShapeRemoved
			}
			return finish(chosen)
		}
		if hashValue(left.Meta.Args) != hashValue(right.Meta.Args) {
			return finish(fail(2, "compare_arguments"))
		}
		if partial(left.Meta) != nil || partial(right.Meta) != nil {
			return finish(fail(4, "compare_incomplete"))
		}
		lp, _ := merged(left.Pages, p)
		rp, _ := merged(right.Pages, p)
		if e := shape(ctx, c, &env.Meta, lp, false); e != nil {
			return finish(e)
		}
		if p.Shape != nil {
			if e := shape(ctx, c, &env.Meta, rp, false); e != nil {
				return finish(e)
			}
		} else if hashValue(keys(lp)) != hashValue(keys(rp)) {
			return finish(fail(2, "compare_shape"))
		}
		jq, e := compile(c.Shadow)
		if e != nil {
			return finish(fail(5, "shadow_policy"))
		}
		lv, e := evaluate(ctx, jq, lp)
		if e != nil {
			return finish(e)
		}
		rv, e := evaluate(ctx, jq, rp)
		if e != nil {
			return finish(e)
		}
		lh, rh := hashValue(lv), hashValue(rv)
		equal := lh == rh
		id, e := randomID()
		if e != nil {
			return finish(e)
		}
		name := "shadow-" + id + ".json"
		log := map[string]any{"at": a.now(), "left_sha256": lh, "right_sha256": rh, "equal": equal, "policy_sha256": hashValue(c.Shadow), "left_fetched_at": left.Meta.FetchedAt, "right_fetched_at": right.Meta.FetchedAt}
		if e := publish(c.RawDir, name, mustJSON(log), a.now()); e != nil {
			return finish(e)
		}
		env.Meta.DiffLog = c.RawDir + string(os.PathSeparator) + name
		env.Meta.BaselineFetchedAt = &right.Meta.FetchedAt
		if !equal {
			return finish(fail(2, "compare_difference"))
		}
		env.Data = []any{map[string]any{"equal": true}}
		return finish(nil)
	}
	return finish(fail(1, "unknown_command"))
}
func prefer(errs ...error) error {
	best := error(nil)
	rank := map[int]int{1: 6, 5: 5, 3: 4, 2: 3, 6: 2, 4: 1}
	for _, e := range errs {
		if e != nil && (best == nil || rank[errCode(e)] > rank[errCode(best)]) {
			best = e
		}
	}
	return best
}
func partial(m Meta) error {
	if m.Complete != "true" {
		return fail(4, "incomplete")
	}
	return nil
}
func filter(ctx context.Context, o options, p Policy, pages []any, m *Meta) ([]any, error) {
	v, e := merged(pages, p)
	if e != nil {
		return nil, e
	}
	if e := expectOK(ctx, o.expect, v); e != nil {
		return nil, e
	}
	values, e := evaluate(ctx, o.jq, v)
	if e != nil {
		return nil, e
	}
	out := []any{}
	size := 2
	for _, value := range values {
		if value == nil {
			return nil, fail(2, "jq_null")
		}
		b, e := json.Marshal(value)
		if e != nil {
			return nil, fail(2, "result_encode")
		}
		extra := len(b)
		if len(out) > 0 {
			extra++
		}
		if size+extra > o.maxBytes {
			m.Complete = "false"
			break
		}
		out = append(out, value)
		size += extra
	}
	m.Matched = len(out)
	if len(out) == 0 {
		m.TopKeys = keys(v)
	}
	return out, nil
}
func (a *App) fetch(ctx context.Context, c Config, cl *client, o options, p Policy) (snap Snapshot, data []any, err error) {
	snap.Meta = Meta{Server: o.server, Tool: o.tool, Args: o.args, SchemaHash: p.SchemaHash, Complete: "unknown"}
	defer func() {
		snap.Meta.HTTPRequests = cl.requests
		snap.Meta.ToolCalls = cl.calls
		snap.Meta.ResponseBytes = cl.bytes
		if err != nil && len(snap.Pages) > 0 {
			snap.Status = "rejected"
			snap.Reason = brief(err)
			snap.Meta.Reason = brief(err)
			snap.Meta.Code = errCode(err)
			// Preserve received results even when the network deadline has expired.
			if saveErr := a.save(context.WithoutCancel(ctx), c, &snap); saveErr != nil {
				snap.Meta.Raw = ""
				snap.Meta.RawID = ""
				snap.Meta.RawSHA256 = ""
				err = prefer(err, saveErr)
			}
		}
	}()
	args := map[string]any{}
	for k, v := range o.args {
		args[k] = v
	}
	max := 1
	if p.Page != nil {
		max = p.Page.MaxPages
	}
	seen := map[string]bool{}
	for i := 0; i < max; i++ {
		if _, _, e := authorize(c, o.server, o.tool, args, i > 0); e != nil {
			return snap, nil, e
		}
		v, e := cl.rpc(ctx, "tools/call", map[string]any{"name": o.tool, "arguments": args})
		if e != nil {
			return snap, nil, e
		}
		snap.Pages = append(snap.Pages, v)
		vtarget, blocks, e := target(v)
		snap.Meta.ContentBlocks += blocks
		if e != nil {
			return snap, nil, e
		}
		if e = expectOK(ctx, o.expect, vtarget); e != nil {
			return snap, nil, e
		}
		if p.Page == nil {
			snap.Meta.Complete = declaredCompleteness(ctx, p, vtarget, o.args)
			break
		}
		page := p.Page
		items, has := getPath(vtarget, page.Items)
		arr, ok := items.([]any)
		if !has || !ok {
			return snap, nil, fail(2, "page_items")
		}
		next, has := getPath(vtarget, page.Next)
		if !has {
			return snap, nil, fail(2, "page_next_missing")
		}
		if next == nil || next == "" {
			limit, ok := args[page.LimitArg].(json.Number)
			if !ok {
				return snap, nil, fail(2, "page_limit")
			}
			n, e := strconv.Atoi(limit.String())
			if e != nil || n < 1 {
				return snap, nil, fail(2, "page_limit")
			}
			snap.Meta.Complete = "true"
			if len(arr) >= n {
				snap.Meta.Complete = "false"
			}
			break
		}
		cursor, ok := next.(string)
		if !ok || len(cursor) > 4096 {
			return snap, nil, fail(2, "page_cursor")
		}
		snap.Meta.Complete = "false"
		if seen[cursor] {
			break
		}
		seen[cursor] = true
		args[page.CursorArg] = cursor
	}
	v, e := merged(snap.Pages, p)
	if e != nil {
		return snap, nil, e
	}
	if e = expectOK(ctx, o.expect, v); e != nil {
		return snap, nil, e
	}
	if e = shape(ctx, c, &snap.Meta, v, o.acceptShape); e != nil {
		return snap, nil, e
	}
	snap.Meta.HTTPRequests = cl.requests
	snap.Meta.ToolCalls = cl.calls
	snap.Meta.ResponseBytes = cl.bytes
	// Budget completeness is persisted so query cannot turn a partial call into success.
	temp := snap.Meta
	if data, e = filter(ctx, o, p, snap.Pages, &temp); e != nil {
		return snap, nil, e
	}
	snap.Meta = temp
	if e = a.save(ctx, c, &snap); e != nil {
		return snap, nil, e
	}
	return snap, data, nil
}

// Prevent accidental exposure of transport/config errors in the CLI.
func main() {
	a := &App{}
	env, code := a.Run(os.Args[1:])
	b, e := json.Marshal(env)
	if e != nil {
		b = []byte(`{"meta":{"code":2,"reason":"output_encode"}}`)
		code = 2
	}
	if _, e = os.Stdout.Write(append(b, '\n')); e != nil {
		code = 2
	}
	os.Exit(code)
}
