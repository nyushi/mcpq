package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type client struct {
	http     *http.Client
	server   Server
	endpoint string
	session  string
	seq      int
	bytes    int64
	requests int
	calls    int
}

func newClient(s Server) (*client, error) {
	endpoint := os.Getenv(s.EndpointEnv)
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fail(5, "endpoint_invalid")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fail(5, "endpoint_requires_tls")
	}
	return &client{server: s, endpoint: endpoint, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *client) rpc(ctx context.Context, method string, params any) (any, error) {
	c.seq++
	id := c.seq
	body := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	notification := method == "notifications/initialized"
	if !notification {
		body["id"] = id
	}
	b, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if e != nil {
		return nil, fail(2, "request_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.seq > 1 {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.server.TokenEnv != "" {
		tok := os.Getenv(c.server.TokenEnv)
		if tok == "" {
			return nil, fail(3, "token_missing")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	c.requests++
	if method == "tools/call" {
		c.calls++
	}
	res, e := c.http.Do(req)
	if e != nil {
		return nil, fail(2, "transport_failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, fail(3, "authentication")
	}
	if notification {
		if res.StatusCode != 202 {
			return nil, fail(2, "notification_rejected")
		}
		return nil, nil
	}
	if res.StatusCode != 200 {
		return nil, fail(2, "http_status")
	}
	if method == "initialize" {
		c.session = res.Header.Get("Mcp-Session-Id")
		for _, r := range c.session {
			if r < 33 || r > 126 {
				return nil, fail(2, "session_invalid")
			}
		}
	}
	media, _, e := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if e != nil {
		return nil, fail(2, "content_type")
	}
	reader := &countReader{r: io.LimitReader(res.Body, receiveLimit+1), count: &c.bytes}
	if media == "application/json" {
		raw, e := io.ReadAll(reader)
		if e != nil || len(raw) > receiveLimit {
			return nil, fail(2, "receive_limit")
		}
		v, e := decode(raw)
		if e != nil {
			return nil, e
		}
		r, found, e := response(v, id)
		if !found && e == nil {
			return nil, fail(2, "missing_result")
		}
		return r, e
	}
	if media != "text/event-stream" {
		return nil, fail(2, "content_type")
	}
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), receiveLimit)
	event := []string{}
	total := 0
	for scan.Scan() {
		line := scan.Text()
		total += len(line) + 1
		if total > receiveLimit {
			return nil, fail(2, "receive_limit")
		}
		if line == "" {
			if len(event) > 0 {
				v, e := decode([]byte(strings.Join(event, "\n")))
				if e != nil {
					return nil, e
				}
				r, found, e := response(v, id)
				if e != nil {
					return nil, e
				}
				if found {
					return r, nil
				}
			}
			event = nil
			continue
		}
		if strings.HasPrefix(line, "data:") {
			event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return nil, fail(2, "stream_disconnected")
}

type countReader struct {
	r     io.Reader
	count *int64
}

func (r *countReader) Read(p []byte) (int, error) {
	n, e := r.r.Read(p)
	*r.count += int64(n)
	return n, e
}
func response(v any, id int) (any, bool, error) {
	m, ok := v.(map[string]any)
	if !ok || m["jsonrpc"] != "2.0" {
		return nil, false, fail(2, "rpc_shape")
	}
	if _, ok := m["method"]; ok {
		if _, hasID := m["id"]; hasID {
			return nil, false, fail(2, "server_request_unsupported")
		}
		return nil, false, nil
	}
	got, ok := m["id"].(json.Number)
	if !ok || got.String() != strconv.Itoa(id) {
		return nil, false, fail(2, "rpc_id")
	}
	if _, ok := m["error"]; ok {
		return nil, false, fail(2, "rpc_error")
	}
	r, ok := m["result"]
	if !ok || r == nil {
		return nil, false, fail(2, "missing_result")
	}
	return r, true, nil
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func (c *client) init(ctx context.Context) error {
	v, e := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "mcpq", "version": "0.1.0"}})
	if e != nil {
		return e
	}
	m, ok := v.(map[string]any)
	if !ok || m["protocolVersion"] != protocolVersion {
		return fail(2, "protocol_version")
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok {
		return fail(2, "capabilities")
	}
	if _, ok := caps["tools"].(map[string]any); !ok {
		return fail(2, "tools_capability")
	}
	_, e = c.rpc(ctx, "notifications/initialized", nil)
	return e
}
func (c *client) tools(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		v, e := c.rpc(ctx, "tools/list", params)
		if e != nil {
			return nil, e
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fail(2, "tools_shape")
		}
		arr, ok := m["tools"].([]any)
		if !ok {
			return nil, fail(2, "tools_shape")
		}
		for _, v := range arr {
			t, ok := v.(map[string]any)
			if !ok {
				return nil, fail(2, "tool_shape")
			}
			name, ok := t["name"].(string)
			if !ok || name == "" {
				return nil, fail(2, "tool_name")
			}
			if _, exists := out[name]; exists {
				return nil, fail(5, "duplicate_tool")
			}
			if _, ok := t["inputSchema"].(map[string]any); !ok {
				return nil, fail(5, "schema_missing")
			}
			out[name] = map[string]any{"name": name, "inputSchema": t["inputSchema"]}
		}
		next, has := m["nextCursor"]
		if !has || next == nil || next == "" {
			return out, nil
		}
		s, ok := next.(string)
		if !ok || len(s) > 4096 || seen[s] {
			return nil, fail(2, "tools_cursor")
		}
		seen[s] = true
		cursor = s
	}
	return nil, fail(2, "tools_page_limit")
}
