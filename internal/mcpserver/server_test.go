package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/ratelimit"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

const key = "mcp-secret-key"

type fixture struct {
	session *mcp.ClientSession
	posts   *atomic.Int32
}

func connect(t *testing.T, readOnly bool) fixture {
	t.Helper()
	posts := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return
		}
		switch r.URL.Path {
		case "/database-info":
			fmt.Fprint(w, `{"release":"2026-11-02T03:00:24.204000Z"}`)
		case "/web":
			fmt.Fprint(w, `{"documents":{"content":{"count":10,"urls":["https://e/1"]},"index":{"count":9,"urls":[]},"error":{"count":0,"urls":[]}},"history":[]}`)
		case "/web/document":
			fmt.Fprintf(w, `{"url":%q,"isIndexed":true,"title":"page title"}`, r.URL.Query().Get("url"))
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"detail":"nothing here"}`)
		}
	}))
	t.Cleanup(srv.Close)

	store := ratelimit.Store{Dir: t.TempDir()}
	opts := Options{
		Version:  "test",
		ReadOnly: readOnly,
		Service: func(profile string) (*wm.Service, error) {
			if profile == "missing" {
				return nil, fmt.Errorf("no such profile")
			}
			c, err := api.New(api.Options{BaseURL: srv.URL, Key: key})
			if err != nil {
				return nil, err
			}
			return &wm.Service{API: c, Ledger: ratelimit.NewLedger(store, "k"), ReadOnly: readOnly}, nil
		},
		Profiles: func() ([]map[string]any, error) {
			return []map[string]any{{"name": "shop", "site": "example.com"}}, nil
		},
	}
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := New(opts).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return fixture{session, posts}
}

func (f fixture) call(t *testing.T, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if strings.Contains(text, key) {
		t.Fatalf("%s leaked the key: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: not JSON: %q", name, text)
	}
	return out, res.IsError
}

func toolNames(t *testing.T, f fixture) map[string]*mcp.Tool {
	t.Helper()
	res, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestToolsMatchCatalogAndAnnotations(t *testing.T) {
	tools := toolNames(t, connect(t, false))
	if len(tools) != len(Catalog()) {
		t.Fatalf("listed %d tools, catalog has %d", len(tools), len(Catalog()))
	}
	for _, info := range Catalog() {
		tool, ok := tools[info.Name]
		if !ok {
			t.Errorf("%s not registered", info.Name)
			continue
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != info.ReadOnly {
			t.Errorf("%s annotation mismatch: %+v", info.Name, tool.Annotations)
		}
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("%s lacks description or schema", info.Name)
		}
	}
	schema, _ := json.Marshal(tools["get_document"].InputSchema)
	for _, want := range []string{`"url"`, `"profile"`, `"brief"`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("get_document schema lacks %s: %s", want, schema)
		}
	}
}

func TestReadOnlyHidesWriteTool(t *testing.T) {
	f := connect(t, true)
	tools := toolNames(t, f)
	if _, ok := tools[toolReindex.Name]; ok {
		t.Fatal("reindex tool visible in read-only mode")
	}
	if _, ok := tools[toolHealth.Name]; !ok {
		t.Fatal("read tools missing")
	}
}

func TestHealthReport(t *testing.T) {
	out, isErr := connect(t, true).call(t, "health_report", map[string]any{})
	if isErr || out["ok"] != true {
		t.Fatalf("%v", out)
	}
	data := out["data"].(map[string]any)
	if data["severity"] == nil || data["totals"] == nil {
		t.Fatalf("data = %v", data)
	}
}

func TestOverviewOmitsURLsByDefault(t *testing.T) {
	f := connect(t, true)
	out, _ := f.call(t, "site_overview", map[string]any{})
	if strings.Contains(fmt.Sprint(out), "https://e/1") {
		t.Fatalf("URLs included: %v", out)
	}
	out, _ = f.call(t, "site_overview", map[string]any{"include_urls": true})
	if !strings.Contains(fmt.Sprint(out), "https://e/1") {
		t.Fatalf("URLs missing: %v", out)
	}
}

func TestDocumentMarksUntrustedFields(t *testing.T) {
	out, _ := connect(t, true).call(t, "get_document", map[string]any{"url": "https://e/a"})
	meta := out["meta"].(map[string]any)
	if len(meta["untrusted_fields"].([]any)) == 0 {
		t.Fatalf("meta = %v", meta)
	}
}

func TestStructuredToolError(t *testing.T) {
	out, isErr := connect(t, true).call(t, "get_document", map[string]any{"url": "not-a-url"})
	if !isErr || out["ok"] != false {
		t.Fatalf("%v", out)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "invalid_argument" || e["field"] != "url" {
		t.Fatalf("error = %v", e)
	}
}

func TestReindexSafety(t *testing.T) {
	f := connect(t, false)
	two := []any{"https://e/1", "https://e/2"}

	out, isErr := f.call(t, "reindex_urls", map[string]any{"urls": two})
	if !isErr || out["error"].(map[string]any)["code"] != "confirmation_required" {
		t.Fatalf("%v", out)
	}
	out, isErr = f.call(t, "reindex_urls", map[string]any{"urls": two, "dry_run": true})
	if isErr || out["data"].(map[string]any)["dry_run"] != true || f.posts.Load() != 0 {
		t.Fatalf("dry run: %v, posts=%d", out, f.posts.Load())
	}
	out, isErr = f.call(t, "reindex_urls", map[string]any{"urls": two, "confirm_bulk": true})
	if isErr || out["ok"] != true || f.posts.Load() != 2 {
		t.Fatalf("submit: %v, posts=%d", out, f.posts.Load())
	}
	quota, _ := f.call(t, "quota_status", map[string]any{})
	if quota["data"].(map[string]any)["used_estimate"] != float64(2) {
		t.Fatalf("quota = %v", quota)
	}
}

func TestBatchSizeLimit(t *testing.T) {
	urls := make([]any, maxBatchURLs+1)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://e/%d", i)
	}
	out, isErr := connect(t, true).call(t, "check_documents", map[string]any{"urls": urls})
	if !isErr || out["error"].(map[string]any)["field"] != "urls" {
		t.Fatalf("%v", out)
	}
}

func TestListProfiles(t *testing.T) {
	out, _ := connect(t, true).call(t, "list_profiles", map[string]any{})
	if !strings.Contains(fmt.Sprint(out), "shop") {
		t.Fatalf("%v", out)
	}
}

func TestUnknownProfileIsReported(t *testing.T) {
	_, isErr := connect(t, true).call(t, "database_info", map[string]any{"profile": "missing"})
	if !isErr {
		t.Fatal("expected an error")
	}
}
