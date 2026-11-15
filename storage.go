package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	tmp, e := os.CreateTemp(root, "pending-")
	if e != nil {
		return fail(2, "store_write")
	}
	path := tmp.Name()
	defer os.Remove(path)
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
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
	if e = os.Link(path, filepath.Join(root, name)); e != nil {
		return fail(2, "store_publish")
	}
	return nil
}
func (a *App) save(ctx context.Context, c Config, s *Snapshot) error {
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
	if _, e = merged(s.Pages, p); e != nil {
		return s, e
	}
	age := a.now().Sub(s.Meta.FetchedAt)
	secs := age.Seconds()
	s.Meta.Age = &secs
	s.Meta.SourceToolCalls = s.Meta.ToolCalls
	s.Meta.SourceHTTPRequests = s.Meta.HTTPRequests
	s.Meta.SourceResponseBytes = s.Meta.ResponseBytes
	s.Meta.ResponseBytes = 0
	s.Meta.ToolCalls = 0
	s.Meta.HTTPRequests = 0
	if ctx.Err() != nil {
		return s, fail(2, "deadline")
	}
	if s.Meta.FetchedAt.IsZero() || !st.ModTime().Equal(s.Meta.FetchedAt) || age < 0 || age > maxAge || age >= 24*time.Hour {
		return s, fail(6, "snapshot_stale")
	}
	return s, nil
}
func shape(ctx context.Context, c Config, m *Meta, v any) error {
	if ctx.Err() != nil {
		return fail(2, "deadline")
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
		return fail(2, "shape_changed")
	}
	m.Shape = "unchanged"
	return nil
}

func snapshotIntegrity(s Snapshot) string {
	s.Integrity = ""
	return hashValue(s)
}
