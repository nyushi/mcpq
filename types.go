package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const defaultConfigPath = "/etc/mcpq/config.json"
const protocolVersion = "2025-06-18"
const receiveLimit = 8 << 20
const outputLimit = 1 << 20

type Config struct {
	RawDir   string            `json:"raw_dir"`
	Deadline string            `json:"deadline"`
	MaxAge   string            `json:"max_age"`
	Shadow   string            `json:"shadow_jq"`
	Servers  map[string]Server `json:"servers"`
}
type Server struct {
	AllowPlainHTTPHosts []string          `json:"allow_plain_http_hosts,omitempty"`
	EndpointEnv         string            `json:"endpoint_env"`
	TokenEnv            string            `json:"token_env"`
	Tools               map[string]Policy `json:"tools"`
}
type Policy struct {
	SchemaHash string           `json:"schema_hash"`
	Args       map[string][]any `json:"args"`
	Required   []string         `json:"required"`
	Page       *Page            `json:"page,omitempty"`
}
type Page struct {
	Items     string `json:"items"`
	Next      string `json:"next"`
	LimitArg  string `json:"limit_arg"`
	CursorArg string `json:"cursor_arg"`
	MaxPages  int    `json:"max_pages"`
}
type Meta struct {
	Server              string         `json:"server,omitempty"`
	Tool                string         `json:"tool,omitempty"`
	Args                map[string]any `json:"args"`
	FetchedAt           time.Time      `json:"fetched_at,omitempty"`
	Age                 *float64       `json:"age,omitempty"`
	ResponseBytes       int64          `json:"response_bytes"`
	ToolCalls           int            `json:"tool_calls"`
	HTTPRequests        int            `json:"http_requests"`
	SourceResponseBytes int64          `json:"source_response_bytes,omitempty"`
	SourceToolCalls     int            `json:"source_tool_calls,omitempty"`
	SourceHTTPRequests  int            `json:"source_http_requests,omitempty"`
	Complete            Completeness   `json:"complete,omitempty"`
	RawID               string         `json:"raw_id,omitempty"`
	Raw                 string         `json:"raw,omitempty"`
	RawSHA256           string         `json:"raw_sha256,omitempty"`
	SchemaHash          string         `json:"schema_hash,omitempty"`
	ContentBlocks       int            `json:"content_blocks"`
	Matched             int            `json:"matched"`
	Shape               string         `json:"shape,omitempty"`
	ShapeAdded          *[]string      `json:"shape_added,omitempty"`
	ShapeRemoved        *[]string      `json:"shape_removed,omitempty"`
	TopKeys             []string       `json:"top_keys,omitempty"`
	Reason              string         `json:"reason,omitempty"`
	Code                int            `json:"code"`
	DiffLog             string         `json:"diff_log,omitempty"`
	BaselineFetchedAt   *time.Time     `json:"baseline_fetched_at,omitempty"`
}
type Envelope struct {
	Meta Meta  `json:"meta"`
	Data []any `json:"data,omitempty"`
}

// Data must distinguish an empty successful stream from a suppressed result.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if e.Data == nil {
		return json.Marshal(struct {
			Meta Meta `json:"meta"`
		}{e.Meta})
	}
	return json.Marshal(struct {
		Meta Meta  `json:"meta"`
		Data []any `json:"data"`
	}{e.Meta, e.Data})
}

type Completeness string

func (c Completeness) MarshalJSON() ([]byte, error) {
	switch c {
	case "true":
		return []byte("true"), nil
	case "false":
		return []byte("false"), nil
	case "unknown":
		return []byte(`"unknown"`), nil
	case "":
		return []byte(`""`), nil
	}
	return nil, fail(2, "complete_invalid")
}
func (c *Completeness) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "true":
		*c = "true"
	case "false":
		*c = "false"
	case `"unknown"`:
		*c = "unknown"
	default:
		return fail(2, "complete_invalid")
	}
	return nil
}

type Snapshot struct {
	Status    string `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Meta      Meta   `json:"meta"`
	Pages     []any  `json:"pages"`
	Integrity string `json:"integrity_sha256"`
}
type App struct {
	ConfigPath string
	Now        func() time.Time
}
type failure struct {
	code   int
	reason string
}

func (e failure) Error() string          { return e.reason }
func fail(code int, reason string) error { return failure{code, reason} }
func errCode(err error) int {
	var f failure
	if errors.As(err, &f) {
		return f.code
	}
	return 2
}
func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}
func hashBytes(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func hashValue(v any) string    { b, _ := json.Marshal(v); return hashBytes(b) }
func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fail(2, "invalid_json")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fail(2, "trailing_json")
	}
	return v, nil
}
func decodeAs(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fail(2, "invalid_document")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fail(2, "trailing_document")
	}
	return nil
}
func jqNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			i, ok := new(big.Int).SetString(string(x), 10)
			if ok {
				return i
			}
		}
		f, e := x.Float64()
		if e != nil {
			return x
		}
		return f
	case []any:
		r := make([]any, len(x))
		for i, v := range x {
			r[i] = jqNumbers(v)
		}
		return r
	case map[string]any:
		r := map[string]any{}
		for k, v := range x {
			r[k] = jqNumbers(v)
		}
		return r
	}
	return v
}
func keys(v any) []string {
	ks := []string{}
	if m, ok := v.(map[string]any); ok {
		for k := range m {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

var validID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
var validHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func checkPath(p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return fail(2, "unsafe_path")
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, cur), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || st.Mode()&os.ModeSymlink != 0 {
			return fail(2, "unsafe_path")
		}
	}
	return nil
}
func safeOpen(p string) (*os.File, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fail(2, "read_failed")
	}
	f := os.NewFile(uintptr(fd), p)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fail(2, "not_regular")
	}
	return f, nil
}
func owner(st os.FileInfo) uint32 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Uid
	}
	return ^uint32(0)
}
func readSafe(p string, max int64) ([]byte, os.FileInfo, error) {
	f, err := safeOpen(p)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }() // Read-only descriptor cleanup.
	st, err := f.Stat()
	if err != nil {
		return nil, nil, fail(2, "stat_failed")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, nil, fail(2, "read_limit")
	}
	return b, st, nil
}
func loadConfig(path string) (Config, error) {
	var c Config
	b, st, err := readSafe(path, receiveLimit)
	if err != nil {
		return c, fail(5, "config_unreadable")
	}
	if st.Mode().Perm()&0022 != 0 || (owner(st) != uint32(os.Geteuid()) && owner(st) != 0) {
		return c, fail(5, "config_permissions")
	}
	if decodeAs(b, &c) != nil {
		return c, fail(5, "config_invalid")
	}
	if c.RawDir == "" || !filepath.IsAbs(c.RawDir) {
		return c, fail(5, "config_raw_dir")
	}
	if c.MaxAge == "" {
		c.MaxAge = "15m"
	}
	age, e := time.ParseDuration(c.MaxAge)
	if e != nil || age <= 0 || age > time.Hour {
		return c, fail(5, "config_max_age")
	}
	if c.Deadline == "" {
		c.Deadline = "30s"
	}
	d, e := time.ParseDuration(c.Deadline)
	if e != nil || d <= 0 || d > 5*time.Minute {
		return c, fail(5, "config_deadline")
	}
	if c.Shadow == "" {
		c.Shadow = "."
	}
	for _, s := range c.Servers {
		for _, host := range s.AllowPlainHTTPHosts {
			if !validPlainHost(host) {
				return c, fail(5, "config_plain_http_host")
			}
		}
		if s.EndpointEnv == "" {
			return c, fail(5, "config_endpoint_env")
		}
		for name, p := range s.Tools {
			if dangerous(name) || !validHash.MatchString(p.SchemaHash) {
				return c, fail(5, "config_policy")
			}
			if p.Page != nil {
				pg := p.Page
				if pg.MaxPages == 0 {
					pg.MaxPages = 20
				}
				if pg.MaxPages < 1 || pg.MaxPages > 100 || pg.Items == "" || pg.Next == "" || pg.LimitArg == "" || pg.CursorArg == "" || pg.LimitArg == pg.CursorArg {
					return c, fail(5, "config_page")
				}
			}
		}
	}
	return c, nil
}
func dangerous(s string) bool {
	s = strings.ToLower(s)
	for _, x := range []string{"create_", "update_", "delete_", "add_", "execute", "manage_"} {
		if strings.HasPrefix(s, x) || strings.Contains(s, "_"+x) {
			return true
		}
	}
	return strings.Contains(s, "api_request") || strings.Contains(s, "manage_")
}
func authorize(c Config, server, tool string, args map[string]any, generated bool) (Server, Policy, error) {
	s, ok := c.Servers[server]
	if !ok {
		return s, Policy{}, fail(5, "server_denied")
	}
	p, ok := s.Tools[tool]
	if !ok || dangerous(tool) {
		return s, p, fail(5, "tool_denied")
	}
	for _, k := range p.Required {
		if _, ok := args[k]; !ok {
			return s, p, fail(5, "required_argument")
		}
	}
	for k, v := range args {
		if !generated && p.Page != nil && k == p.Page.CursorArg {
			return s, p, fail(5, "initial_cursor_denied")
		}
		if generated && p.Page != nil && k == p.Page.CursorArg {
			if str, ok := v.(string); ok && len(str) > 0 && len(str) <= 4096 {
				continue
			}
			return s, p, fail(5, "cursor_denied")
		}
		allowed := false
		for _, x := range p.Args[k] {
			if hashValue(x) == hashValue(v) {
				allowed = true
				break
			}
		}
		if !allowed {
			return s, p, fail(5, "argument_denied")
		}
	}
	return s, p, nil
}
func duration(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
func brief(err error) string {
	var f failure
	if errors.As(err, &f) {
		return f.reason
	}
	return "operation_failed"
}
