package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"regexp"
	"time"

	"github.com/itchyny/gojq"
)

type batchSpec struct {
	Name  string      `json:"name"`
	Vars  any         `json:"vars"`
	Steps []batchStep `json:"steps"`
}
type batchStep struct {
	ID     string         `json:"id"`
	Server string         `json:"server"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Expect string         `json:"expect"`
	Alert  string         `json:"alert"`
}
type BatchStepMeta struct {
	ID        string       `json:"id"`
	Code      int          `json:"code"`
	Complete  Completeness `json:"complete"`
	Matched   int          `json:"matched"`
	RawID     string       `json:"raw_id"`
	FetchedAt *time.Time   `json:"fetched_at"`
}
type batchSession struct {
	cl    *client
	tools map[string]any
	err   error
}

var batchID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Rewrite variable indexing on the parsed tree, leaving strings and comments intact.
func batchExpression(expr string, vars any) (string, error) {
	q, e := gojq.Parse(expr)
	if e != nil {
		return "", fail(1, "jq_syntax")
	}
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Interface {
			if !v.IsNil() {
				walk(v.Elem())
			}
			return
		}
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return
			}
			walk(v.Elem())
			return
		}
		if v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
			return
		}
		if v.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			walk(v.Field(i))
		}
		if !v.CanAddr() {
			return
		}
		t, ok := v.Addr().Interface().(*gojq.Term)
		if !ok || t.Func == nil || t.Func.Name != "$vars" || len(t.SuffixList) == 0 {
			return
		}
		expression := "$vars"
		for _, suffix := range t.SuffixList {
			if suffix.Index == nil || suffix.Index.IsSlice {
				expression += suffix.String()
				continue
			}
			index := suffix.Index
			key := ""
			if index.Name != "" {
				key = string(mustJSON(index.Name))
			} else if index.Str != nil {
				key = index.Str.String()
			} else if index.Start != nil {
				key = index.Start.String()
			}
			if key == "" {
				expression += suffix.String()
				continue
			}
			expression = "(" + expression + " | . as $batch_value | (" + key + ") as $batch_key | if has($batch_key) then $batch_value[$batch_key] else error(\"missing_batch_variable\") end)"
		}
		replacement, err := gojq.Parse(expression)
		if err == nil {
			*t = gojq.Term{Type: gojq.TermTypeQuery, Query: replacement}
		}
	}
	walk(reflect.ValueOf(q))
	return "(" + string(mustJSON(vars)) + ") as $vars | (" + q.String() + ")", nil
}
func (a *App) runBatch(args []string) (env Envelope, code int) {
	env.Meta.Verdict = "error"
	finish := func(err error) (Envelope, int) {
		code := 0
		if err != nil {
			code = errCode(err)
			env.Meta.Reason = brief(err)
		}
		env.Meta.Code = code
		switch code {
		case 0:
			env.Meta.Verdict = "clear"
		case 7:
			env.Meta.Verdict = "alert"
		default:
			env.Meta.Verdict = "error"
		}
		return env, code
	}
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--allow-partial") {
		return finish(fail(1, "batch_arguments"))
	}
	allow := len(args) == 2
	b, st, e := readSafe(args[0], 1<<20)
	if e != nil || st.Mode().Perm()&0022 != 0 || owner(st) != uint32(os.Geteuid()) {
		return finish(fail(5, "spec_permissions"))
	}
	sum := sha256.Sum256(b)
	env.Meta.SpecSHA256 = hex.EncodeToString(sum[:])
	var spec batchSpec
	if decodeAs(b, &spec) != nil || len(spec.Steps) < 1 || len(spec.Steps) > 20 {
		return finish(fail(1, "spec_invalid"))
	}
	env.Meta.Spec = &spec.Name
	opts := make([]options, len(spec.Steps))
	seen := map[string]bool{}
	for i, s := range spec.Steps {
		if !batchID.MatchString(s.ID) || seen[s.ID] || s.Server == "" || s.Tool == "" || s.Expect == "" || s.Alert == "" {
			return finish(fail(1, "step_invalid"))
		}
		seen[s.ID] = true
		if s.Args == nil {
			s.Args = map[string]any{}
		}
		o, err := parse([]string{"call", s.Server, s.Tool, "--args", string(mustJSON(s.Args)), "--expect", s.Expect, "--jq", "."})
		if err != nil {
			return finish(err)
		}
		// Bind the entire constant as a literal; field references above remain strict.
		expression, err := batchExpression(s.Alert, spec.Vars)
		if err != nil {
			return finish(err)
		}
		o.jq, err = compile(expression)
		if err != nil {
			return finish(err)
		}
		o.alertObjects = true
		o.allow = allow
		opts[i] = o
	}
	path := a.ConfigPath
	if path == "" {
		path = defaultConfigPath
	}
	c, e := loadConfig(path)
	if e != nil {
		return finish(e)
	}
	if os.Getenv("MCPQ_DISABLE") == "1" {
		return finish(fail(5, "disabled"))
	}
	policies := make([]Policy, len(opts))
	for i, o := range opts {
		_, p, err := authorize(c, o.server, o.tool, o.args, false)
		if err != nil {
			return finish(err)
		}
		policies[i] = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration(c.Deadline))
	defer cancel()
	if e = prepareStore(c.RawDir, a.now()); e != nil {
		return finish(e)
	}
	sessions := map[string]*batchSession{}
	defer func() {
		for _, s := range sessions {
			if s.cl != nil {
				env.Meta.HTTPRequests += s.cl.requests
				env.Meta.ToolCalls += s.cl.calls
				env.Meta.ResponseBytes += s.cl.bytes
				s.cl.http.CloseIdleConnections()
			}
		}
	}()
	var overall error
	alerts := []any{}
	partialAlerts := []any{}
	for i, o := range opts {
		s := sessions[o.server]
		if s == nil {
			s = &batchSession{}
			sessions[o.server] = s
			s.cl, s.err = newClient(c.Servers[o.server])
			if s.err == nil {
				s.tools, s.err = s.cl.catalog(ctx)
			}
		}
		err := s.err
		var snap Snapshot
		var data []any
		if err == nil {
			err = checkToolSchema(s.tools, o.tool, policies[i])
		}
		if err == nil {
			snap, data, err = a.fetch(ctx, c, s.cl, o, policies[i])
			if err == nil {
				err = partial(snap.Meta)
			}
		}
		m := BatchStepMeta{ID: spec.Steps[i].ID, Complete: snap.Meta.Complete, Matched: snap.Meta.Matched, RawID: snap.Meta.RawID}
		if !snap.Meta.FetchedAt.IsZero() {
			at := snap.Meta.FetchedAt
			m.FetchedAt = &at
		}
		if err != nil {
			m.Code = errCode(err)
		}
		env.Meta.Steps = append(env.Meta.Steps, m)
		overall = prefer(overall, err)
		for _, v := range data {
			item := map[string]any{"step": m.ID, "alert": v}
			switch m.Code {
			case 0:
				alerts = append(alerts, item)
			case 4:
				partialAlerts = append(partialAlerts, item)
			}
		}
	}
	if overall == nil {
		env.Data = alerts
		if len(alerts) > 0 {
			overall = fail(7, "alerts")
		}
	} else if errCode(overall) == 4 && allow {
		env.Data = partialAlerts
	}
	return finish(overall)
}
