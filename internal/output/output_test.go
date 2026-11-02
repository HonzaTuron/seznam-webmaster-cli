package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

func TestSelect(t *testing.T) {
	data := Normalize(map[string]any{
		"summary": map[string]any{"total": 2, "failed": 0},
		"items":   []any{map[string]any{"url": "a", "state": "x", "more": 1}, map[string]any{"url": "b", "state": "y"}},
		"quota":   map[string]any{"limit": 500},
	})
	got := Select(data, []string{"summary.total", "items.url", "missing.path"})
	raw, _ := json.Marshal(got)
	want := `{"items":[{"url":"a"},{"url":"b"}],"summary":{"total":2}}`
	if string(raw) != want {
		t.Fatalf("got %s", raw)
	}
}

func TestSelectOnTopLevelArray(t *testing.T) {
	got := Select(Normalize([]any{map[string]any{"a": 1, "b": 2}}), []string{"a"})
	raw, _ := json.Marshal(got)
	if string(raw) != `[{"a":1}]` {
		t.Fatalf("got %s", raw)
	}
}

func run(f Format, env Envelope, v View, fields ...string) (string, string) {
	var out, errs bytes.Buffer
	w := &Writer{Format: f, Stdout: &out, Stderr: &errs, Fields: fields}
	w.Final(env, v)
	return out.String(), errs.String()
}

func base() Envelope {
	return Envelope{SchemaVersion: SchemaVersion, OK: true, Command: "x", Meta: Meta{Version: "t"}}
}

func TestJSONFailureIsStillOneDocument(t *testing.T) {
	env := base()
	env.OK = false
	env.Error = apperr.New(apperr.CodeAuthInvalid, "nope").WithHint("do this")
	out, errs := run(JSON, env, View{})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if parsed["ok"] != false || parsed["error"].(map[string]any)["code"] != "auth_invalid" {
		t.Fatalf("parsed = %v", parsed)
	}
	if !strings.Contains(errs, "auth_invalid") || !strings.Contains(errs, "do this") {
		t.Fatalf("stderr = %q", errs)
	}
}

func TestURLsAreNotHTMLEscaped(t *testing.T) {
	env := base()
	env.Data = map[string]any{"url": "https://e/?a=1&b=2"}
	out, _ := run(JSON, env, View{})
	if !strings.Contains(out, "a=1&b=2") {
		t.Fatalf("escaped: %s", out)
	}
}

func TestNDJSONEndsWithSummary(t *testing.T) {
	env := base()
	env.Data = map[string]any{"summary": map[string]any{"total": 2}}
	out, _ := run(NDJSON, env, View{Rows: []map[string]any{{"url": "a"}, {"url": "b"}}})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	var first, last map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[2]), &last)
	if first["type"] != "item" || first["url"] != "a" {
		t.Fatalf("first = %v", first)
	}
	if last["type"] != "summary" || last["ok"] != true || last["summary"] == nil {
		t.Fatalf("last = %v", last)
	}
}

func TestStreamedRowsAreNotRepeated(t *testing.T) {
	env := base()
	out, _ := run(NDJSON, env, View{Rows: []map[string]any{{"url": "a"}}, Streamed: true})
	if strings.Contains(out, `"url"`) {
		t.Fatalf("row printed twice: %s", out)
	}
}

func TestRawPassesBodyThroughUnchanged(t *testing.T) {
	env := base()
	body := `{"keep":   "spacing"}`
	out, _ := run(JSON, env, View{Raw: []byte(body)})
	if out != body+"\n" {
		t.Fatalf("got %q", out)
	}
}

func TestRawDoesNotHideFailures(t *testing.T) {
	env := base()
	env.OK = false
	env.Error = apperr.New(apperr.CodeNotFound, "gone")
	out, _ := run(JSON, env, View{Raw: []byte(`{"partial":true}`)})
	if !strings.Contains(out, "not_found") {
		t.Fatalf("got %q", out)
	}
}

func TestTableAndCSV(t *testing.T) {
	env := base()
	rows := []map[string]any{{"date": "2026-01-01", "indexed": json.Number("5")}}
	v := View{Rows: rows, Columns: []string{"date", "indexed"}}
	out, _ := run(Table, env, v)
	if !strings.Contains(out, "DATE") || !strings.Contains(out, "2026-01-01") {
		t.Fatalf("table = %q", out)
	}
	out, _ = run(CSV, env, v)
	if out != "date,indexed\n2026-01-01,5\n" {
		t.Fatalf("csv = %q", out)
	}
}

func TestTableFlattensObjects(t *testing.T) {
	env := base()
	env.Data = map[string]any{"a": map[string]any{"b": 1}, "list": []any{1, 2}}
	out, _ := run(Table, env, View{})
	if !strings.Contains(out, "a.b") || !strings.Contains(out, "[2 items]") {
		t.Fatalf("table = %q", out)
	}
}

func TestParseFormat(t *testing.T) {
	if _, err := ParseFormat("xml"); err == nil {
		t.Fatal("accepted xml")
	}
}
