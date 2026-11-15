package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var managedFile = regexp.MustCompile(`^([a-f0-9]{32}\.json|shadow-[a-f0-9]{32}\.json|pending-[0-9]+)$`)
var shapeFile = regexp.MustCompile(`^shape-[a-f0-9]{64}\.json$`)

func prepareStore(root string, now time.Time) error {
	if e := checkPath(root); e != nil {
		return e
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return fail(2, "store_create")
	}
	st, e := os.Lstat(root)
	if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 || owner(st) != uint32(os.Geteuid()) {
		return fail(2, "store_permissions")
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		return fail(2, "store_read")
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		st, e := os.Lstat(path)
		if e != nil {
			return fail(2, "store_stat")
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fail(2, "store_symlink")
		}
		if !st.Mode().IsRegular() || owner(st) != uint32(os.Geteuid()) {
			return fail(2, "store_entry")
		}
		if shapeFile.MatchString(entry.Name()) {
			continue
		}
		if !managedFile.MatchString(entry.Name()) {
			return fail(2, "store_unknown_entry")
		}
		if now.Sub(st.ModTime()) >= 24*time.Hour {
			if !st.Mode().IsRegular() {
				return fail(2, "store_entry")
			}
			if e := os.Remove(path); e != nil {
				return fail(2, "store_cleanup")
			}
		}
	}
	return nil
}
func randomID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", fail(2, "random_failed")
	}
	return hex.EncodeToString(b[:]), nil
}
func publish(root, name string, b []byte, at time.Time) error {
	return publishFile(root, name, b, at, false)
}
func publishFile(root, name string, b []byte, at time.Time, replace bool) error {
	tmp, e := os.CreateTemp(root, "pending-")
	if e != nil {
		return fail(2, "store_write")
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }() // Temporary cleanup is best effort after publication.
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
		}
	}()
	if e = tmp.Chmod(0600); e != nil {
		return fail(2, "store_permissions")
	}
	if _, e = tmp.Write(b); e != nil {
		return fail(2, "store_write")
	}
	if e = tmp.Sync(); e != nil {
		return fail(2, "store_sync")
	}
	if e = tmp.Close(); e != nil {
		return fail(2, "store_close")
	}
	ok = true
	if e = os.Chtimes(path, at, at); e != nil {
		return fail(2, "store_time")
	}
	if replace {
		e = os.Rename(path, filepath.Join(root, name))
	} else {
		e = os.Link(path, filepath.Join(root, name))
	}
	if e != nil {
		return fail(2, "store_publish")
	}
	return nil
}
func (a *App) save(ctx context.Context, c Config, original *Snapshot) error {
	copySnapshot := *original
	s := &copySnapshot
	if ctx.Err() != nil {
		return fail(2, "deadline")
	}
	id, e := randomID()
	if e != nil {
		return e
	}
	s.Meta.RawID = id
	s.Meta.Raw = filepath.Join(c.RawDir, id+".json")
	s.Meta.RawSHA256 = hashValue(s.Pages)
	s.Meta.FetchedAt = a.now()
	s.Integrity = snapshotIntegrity(*s)
	b, e := json.Marshal(s)
	if e != nil {
		return fail(2, "snapshot_encode")
	}
	if len(b) > receiveLimit*100 {
		return fail(2, "snapshot_limit")
	}
	if e = publish(c.RawDir, id+".json", b, s.Meta.FetchedAt); e != nil {
		return e
	}
	if ctx.Err() != nil {
		return fail(2, "deadline")
	}
	*original = *s
	return nil
}
func readSnapshot(c Config, id string) (Snapshot, os.FileInfo, error) {
	var s Snapshot
	if !validID.MatchString(id) {
		return s, nil, fail(1, "raw_id_invalid")
	}
	path := filepath.Join(c.RawDir, id+".json")
	b, st, e := readSafe(path, receiveLimit*100)
	if e != nil {
		return s, nil, e
	}
	if st.Mode().Perm() != 0600 || owner(st) != uint32(os.Geteuid()) {
		return s, nil, fail(2, "snapshot_permissions")
	}
	if e = decodeAs(b, &s); e != nil {
		return s, nil, e
	}
	if s.Integrity != snapshotIntegrity(s) {
		return s, nil, fail(2, "snapshot_metadata_integrity")
	}
	if s.Meta.RawID != id || s.Meta.Raw != path || s.Meta.RawSHA256 != hashValue(s.Pages) {
		return s, nil, fail(2, "snapshot_integrity")
	}
	return s, st, nil
}
func (a *App) checkSnapshot(ctx context.Context, c Config, id, server, tool string, maxAge time.Duration) (Snapshot, error) {
	s, st, e := readSnapshot(c, id)
	if e != nil {
		return s, e
	}
	s.Meta.SourceToolCalls = s.Meta.ToolCalls
	s.Meta.SourceHTTPRequests = s.Meta.HTTPRequests
	s.Meta.SourceResponseBytes = s.Meta.ResponseBytes
	s.Meta.ResponseBytes = 0
	s.Meta.ToolCalls = 0
	s.Meta.HTTPRequests = 0
	if s.Status == "rejected" {
		return s, fail(2, "snapshot_rejected")
	}
	if s.Status != "" {
		return s, fail(2, "snapshot_status")
	}
	if s.Meta.Server != server || s.Meta.Tool != tool {
		return s, fail(2, "snapshot_target")
	}
	_, p, e := authorize(c, server, tool, s.Meta.Args, false)
	if e != nil {
		return s, e
	}
	if p.SchemaHash != s.Meta.SchemaHash {
		return s, fail(5, "snapshot_schema")
	}
	if s.Meta.Complete != "true" && s.Meta.Complete != "false" && s.Meta.Complete != "unknown" {
		return s, fail(2, "snapshot_complete")
	}
	value, e := merged(s.Pages, p)
	if e != nil {
		return s, e
	}
	if p.Shape != nil {
		if e = shape(ctx, c, &s.Meta, value, false); e != nil {
			return s, e
		}
	}
	age := a.now().Sub(s.Meta.FetchedAt)
	secs := age.Seconds()
	s.Meta.Age = &secs
	if ctx.Err() != nil {
		return s, fail(2, "deadline")
	}
	if s.Meta.FetchedAt.IsZero() || !st.ModTime().Equal(s.Meta.FetchedAt) || age < 0 || age > maxAge || age >= 24*time.Hour {
		return s, fail(6, "snapshot_stale")
	}
	return s, nil
}
func shape(ctx context.Context, c Config, m *Meta, v any, accept bool) error {
	if ctx.Err() != nil {
		return fail(2, "deadline")
	}
	if d := c.Servers[m.Server].Tools[m.Tool].Shape; d != nil {
		if accept {
			return fail(1, "declared_shape_accept")
		}
		return declaredShape(m, v, d)
	}

	name := "shape-" + hashValue([]string{m.Server, m.Tool}) + ".json"
	ks := keys(v)
	b, _ := json.Marshal(ks)
	path := filepath.Join(c.RawDir, name)
	old, st, e := readSafe(path, receiveLimit)
	if e != nil {
		if _, se := os.Lstat(path); os.IsNotExist(se) {
			if e := publish(c.RawDir, name, b, time.Now()); e != nil {
				return e
			}
			m.Shape = "new"
			return nil
		}
		return e
	}
	if st.Mode().Perm() != 0600 || owner(st) != uint32(os.Geteuid()) {
		return fail(2, "shape_permissions")
	}
	var oldKeys []string
	if decodeAs(old, &oldKeys) != nil {
		return fail(2, "shape_invalid")
	}
	if hashValue(oldKeys) != hashValue(ks) {
		m.Shape = "changed"
		added, removed := keyDifference(ks, oldKeys), keyDifference(oldKeys, ks)
		m.ShapeAdded = &added
		m.ShapeRemoved = &removed
		if !accept {
			return fail(2, "shape_changed")
		}
		id, e := randomID()
		if e != nil {
			return e
		}
		name := "shadow-" + id + ".json"
		log := map[string]any{"at": time.Now().UTC(), "old_keys": oldKeys, "new_keys": ks, "added_keys": m.ShapeAdded, "removed_keys": m.ShapeRemoved}
		if e = publish(c.RawDir, name, mustJSON(log), time.Now()); e != nil {
			return e
		}
		m.DiffLog = filepath.Join(c.RawDir, name)
		if e = publishFile(c.RawDir, filepath.Base(path), b, time.Now(), true); e != nil {
			return e
		}
		return nil
	}
	m.Shape = "unchanged"
	return nil
}

func declaredShape(m *Meta, v any, d *ShapePolicy) error {
	m.Shape = "declared"
	m.ShapeAdded, m.ShapeRemoved = nil, nil
	ks := keys(v)
	added := keyDifference(ks, append(append([]string{}, d.Required...), d.Optional...))
	removed := keyDifference(d.Required, ks)
	sort.Strings(removed)
	if len(added) > 0 || len(removed) > 0 {
		m.ShapeAdded, m.ShapeRemoved = &added, &removed
		return fail(2, "shape_changed")
	}
	return nil
}

func snapshotIntegrity(s Snapshot) string {
	s.Integrity = ""
	return hashValue(s)
}

func keyDifference(left, right []string) []string {
	out := []string{}
	for _, k := range left {
		found := false
		for _, r := range right {
			if k == r {
				found = true
				break
			}
		}
		if !found {
			out = append(out, k)
		}
	}
	return out
}

// External files contain a single tools/call result. Target and arguments are
// asserted by the caller; no provenance is inferred from the JSON document.
func (a *App) readBaseline(ctx context.Context, o options, p Policy, left Meta) (Snapshot, error) {
	s := Snapshot{Meta: Meta{Server: o.server, Tool: o.tool, Args: left.Args, FetchedAt: o.baselineAt, Complete: "unknown"}}
	b, st, e := readSafe(o.baselineFile, receiveLimit)
	if e != nil {
		return s, e
	}
	if owner(st) != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 {
		return s, fail(2, "baseline_permissions")
	}
	v, e := decode(b)
	if e != nil {
		return s, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return s, fail(2, "baseline_result")
	}
	_, structured := m["structuredContent"]
	_, content := m["content"]
	if !structured && !content {
		return s, fail(2, "baseline_result")
	}
	value, blocks, e := target(v)
	if e != nil {
		return s, e
	}
	s.Pages = []any{v}
	s.Meta.ContentBlocks = blocks
	if p.Shape != nil {
		if e = declaredShape(&s.Meta, value, p.Shape); e != nil {
			return s, e
		}
	}
	s.Meta.Complete = declaredCompleteness(ctx, p, value, left.Args)
	if p.Page != nil {
		s.Meta.Complete = "true"
		items, has := getPath(value, p.Page.Items)
		arr, ok := items.([]any)
		if !has || !ok {
			return s, fail(2, "page_items")
		}
		next, has := getPath(value, p.Page.Next)
		if !has {
			return s, fail(2, "page_next_missing")
		}
		if next != nil && next != "" {
			if _, ok := next.(string); !ok {
				return s, fail(2, "page_cursor")
			}
			s.Meta.Complete = "false"
		}
		limit, ok := left.Args[p.Page.LimitArg].(json.Number)
		if !ok {
			return s, fail(2, "page_limit")
		}
		n, e := strconv.Atoi(limit.String())
		if e != nil || n < 1 {
			return s, fail(2, "page_limit")
		}
		if len(arr) >= n {
			s.Meta.Complete = "false"
		}
	}
	if ctx.Err() != nil {
		return s, fail(2, "deadline")
	}
	age := a.now().Sub(o.baselineAt)
	if age < 0 || age > o.maxAge {
		return s, fail(6, "baseline_stale")
	}
	return s, nil
}
