package wm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	v, err := api.Decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

const freshRelease = "2026-11-02T03:00:24.204000Z"

func webJSON(content, index, errs int, history string) string {
	return fmt.Sprintf(`{"documents":{
		"content":{"count":%d,"urls":[]},"index":{"count":%d,"urls":[]},
		"redirect":{"count":0,"urls":[]},"error":{"count":%d,"urls":["https://e/1"]}},
		"history":[%s]}`, content, index, errs, history)
}

func day(date string, indexed, errs int) string {
	return fmt.Sprintf(`{"date":%q,"counts":{"indexed":%d,"error":%d,"redirected":0,"downloaded":100}}`, date, indexed, errs)
}

func codes(r Report) map[string]string {
	out := map[string]string{}
	for _, f := range r.Findings {
		out[f.Code] = f.Severity
	}
	return out
}

func TestEvaluateHealthySite(t *testing.T) {
	h := day("2026-10-26", 90, 1) + "," + day("2026-11-02", 92, 1)
	r := Evaluate(decode(t, webJSON(100, 92, 1, h)), freshRelease, 7, now)
	if r.Severity != SeverityOK || len(r.Findings) != 0 {
		t.Fatalf("got %s %+v", r.Severity, r.Findings)
	}
	if r.Trend == nil || r.Trend.From != "2026-10-26" || r.Trend.Metrics["indexed"].Delta != 2 {
		t.Fatalf("trend = %+v", r.Trend)
	}
	if r.Ratios["index_of_content"] != 0.92 {
		t.Fatalf("ratio = %v", r.Ratios)
	}
}

func TestEvaluateFindings(t *testing.T) {
	stable := day("2026-10-26", 90, 1) + "," + day("2026-11-02", 90, 1)
	cases := []struct {
		name, web, release, code, severity string
	}{
		{"nothing indexed", webJSON(50, 0, 0, stable), freshRelease, "nothing_indexed", SeverityCritical},
		{"low ratio", webJSON(100, 40, 0, stable), freshRelease, "low_index_ratio", SeverityWarning},
		{"high errors", webJSON(100, 90, 15, stable), freshRelease, "high_error_ratio", SeverityWarning},
		{"very high errors", webJSON(100, 90, 40, stable), freshRelease, "high_error_ratio", SeverityCritical},
		{"index drop", webJSON(100, 80, 1, day("2026-10-26", 100, 1)+","+day("2026-11-02", 85, 1)), freshRelease, "index_drop", SeverityWarning},
		{"index collapse", webJSON(100, 60, 1, day("2026-10-26", 100, 1)+","+day("2026-11-02", 60, 1)), freshRelease, "index_drop", SeverityCritical},
		{"error rise", webJSON(100, 90, 1, day("2026-10-26", 90, 30)+","+day("2026-11-02", 90, 45)), freshRelease, "error_rise", SeverityWarning},
		{"errors from zero", webJSON(100, 90, 1, day("2026-10-26", 90, 0)+","+day("2026-11-02", 90, 20)), freshRelease, "error_rise", SeverityCritical},
		{"no history", webJSON(100, 90, 1, ""), freshRelease, "no_history", SeverityInfo},
		{"stale release", webJSON(100, 90, 1, stable), "2026-10-20T03:00:00Z", "stale_release", SeverityWarning},
		{"stale history", webJSON(100, 90, 1, day("2026-10-01", 90, 1)+","+day("2026-10-08", 90, 1)), freshRelease, "stale_history", SeverityWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(decode(t, tc.web), tc.release, 7, now)
			if got := codes(r)[tc.code]; got != tc.severity {
				t.Fatalf("%s = %q, want %q; findings %+v", tc.code, got, tc.severity, r.Findings)
			}
		})
	}
}

func TestSmallErrorChangesAreIgnored(t *testing.T) {
	h := day("2026-10-26", 90, 2) + "," + day("2026-11-02", 90, 6)
	r := Evaluate(decode(t, webJSON(100, 90, 1, h)), freshRelease, 7, now)
	if _, ok := codes(r)["error_rise"]; ok {
		t.Fatalf("noise reported as finding: %+v", r.Findings)
	}
}

func TestFindingsAreSortedWorstFirst(t *testing.T) {
	r := Evaluate(decode(t, webJSON(50, 0, 0, "")), freshRelease, 7, now)
	if r.Findings[0].Severity != SeverityCritical || r.Severity != SeverityCritical {
		t.Fatalf("%+v", r.Findings)
	}
}

func TestEvaluateToleratesUnknownShapes(t *testing.T) {
	web := decode(t, `{"documents":{"doc_count":120,"index":{"count":10},"future":"x"},"history":"later"}`)
	r := Evaluate(web, "", 7, now)
	if r.Totals["doc_count"] != 120 || r.Totals["index"] != 10 {
		t.Fatalf("totals = %+v", r.Totals)
	}
}

func TestShapeDocuments(t *testing.T) {
	urls := make([]string, 250)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://e/%d", i)
	}
	raw, _ := json.Marshal(map[string]any{
		"error": map[string]any{"count": 5000, "urls": urls},
		"index": map[string]any{"count": 1, "urls": []string{"https://e/i"}},
	})
	t.Run("limit", func(t *testing.T) {
		m := decode(t, string(raw))
		res := Result{}
		shapeDocuments(m, ViewOptions{Limit: 10}, &res)
		cat := m["error"].(map[string]any)
		if len(cat["urls"].([]any)) != 10 || !res.Truncated {
			t.Fatalf("not cut: truncated=%v", res.Truncated)
		}
		if n, _ := api.Int(cat["count"]); n != 5000 {
			t.Fatalf("count changed: %v", cat["count"])
		}
		if !strings.Contains(strings.Join(res.Hints, " "), "random sample") {
			t.Fatalf("sample note missing: %v", res.Hints)
		}
	})
	t.Run("counts only", func(t *testing.T) {
		m := decode(t, string(raw))
		res := Result{}
		shapeDocuments(m, ViewOptions{CountsOnly: true}, &res)
		if _, ok := m["error"].(map[string]any)["urls"]; ok {
			t.Fatal("urls kept")
		}
		if res.Truncated {
			t.Fatal("counts-only is a request, not a truncation")
		}
	})
	t.Run("category filter", func(t *testing.T) {
		m := decode(t, string(raw))
		res := Result{}
		shapeDocuments(m, ViewOptions{Categories: []string{"error", "nope"}}, &res)
		if _, ok := m["index"]; ok {
			t.Fatal("filter ignored")
		}
		if len(res.Warnings) != 1 {
			t.Fatalf("warnings = %v", res.Warnings)
		}
	})
}

func TestCleanURLs(t *testing.T) {
	got, err := CleanURLs([]string{"https://e/a", "https://e/a", " https://e/b "})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, in := range [][]string{
		{},
		{"example.com/x"},
		{"ftp://e/x"},
		{"https://e/a b"},
		{"https://user:pw@e/"},
		{"https://e/ok", "bad", "worse"},
		{"https://e/" + strings.Repeat("x", 3000)},
		{"https://e/\x00"},
	} {
		if _, err := CleanURLs(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestParseLast(t *testing.T) {
	cases := map[string]string{"30d": "2026-10-03", "2w": "2026-10-19", "1d": "2026-11-01"}
	for in, want := range cases {
		if got, err := ParseLast(in, now); err != nil || got != want {
			t.Errorf("%s = %s, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "30", "d", "1y", "-1d"} {
		if _, err := ParseLast(in, now); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestDocumentState(t *testing.T) {
	cases := []struct{ json, want string }{
		{`{"isIndexed":true}`, StateIndexed},
		{`{"isIndexed":false}`, StateNotIndexed},
		{`{}`, StateNotIndexed},
		{`{"isRedirect":true,"isIndexed":true}`, StateRedirect},
		{`{"isError":true,"isIndexed":true}`, StateError},
	}
	for _, tc := range cases {
		if got := documentState(decode(t, tc.json)); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.json, got, tc.want)
		}
	}
}

func TestTimestampUnits(t *testing.T) {
	want := "2026-11-02T03:00:24Z"
	secs := time.Date(2026, 11, 2, 3, 0, 24, 0, time.UTC).Unix()
	for _, n := range []int64{secs, secs * 1000} {
		if got := timestamp(json.Number(fmt.Sprint(n))); got != want {
			t.Errorf("%d = %s", n, got)
		}
	}
	if got := timestamp(json.Number("0")); got != "" {
		t.Errorf("zero = %q", got)
	}
}

func TestEstimateSeconds(t *testing.T) {
	cases := map[int]int{0: 0, 5: 1, 100: 20, 500: 300}
	for n, want := range cases {
		if got := estimateSeconds(n); got != want {
			t.Errorf("estimate(%d) = %d, want %d", n, got, want)
		}
	}
}
