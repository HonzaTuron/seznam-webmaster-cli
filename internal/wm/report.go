package wm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

// Finding severities, ordered from least to most serious.
const (
	SeverityOK       = "ok"
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// SeverityRank orders severities; unknown values rank below everything.
func SeverityRank(s string) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityCritical:
		return 3
	case SeverityOK:
		return 0
	}
	return -1
}

// Thresholds are documented heuristics, not Seznam rules.
const (
	indexDropWarn    = -10.0
	indexDropCrit    = -30.0
	errorRiseWarn    = 20.0
	errorRiseCrit    = 100.0
	errorRiseMinAbs  = 10
	errorRatioWarn   = 0.10
	errorRatioCrit   = 0.25
	lowIndexRatio    = 0.50
	staleDataDays    = 3.0
	errorSampleLimit = 20
)

// Finding is one deterministic observation with a stable code.
type Finding struct {
	Code     string  `json:"code"`
	Severity string  `json:"severity"`
	Message  string  `json:"message"`
	Metric   string  `json:"metric,omitempty"`
	Value    float64 `json:"value,omitempty"`
}

// MetricChange compares a metric between two days.
type MetricChange struct {
	From    int64    `json:"from"`
	To      int64    `json:"to"`
	Delta   int64    `json:"delta"`
	Percent *float64 `json:"percent,omitempty"`
}

// Trend compares the newest history row with one about window days older.
type Trend struct {
	From    string                  `json:"from"`
	To      string                  `json:"to"`
	Metrics map[string]MetricChange `json:"metrics"`
}

// Report is the site health summary.
type Report struct {
	Severity    string             `json:"severity"`
	GeneratedAt string             `json:"generated_at"`
	DataRelease string             `json:"data_release,omitempty"`
	Totals      map[string]int64   `json:"totals"`
	Ratios      map[string]float64 `json:"ratios"`
	Trend       *Trend             `json:"trend,omitempty"`
	Findings    []Finding          `json:"findings"`
	ErrorSample []any              `json:"error_sample,omitempty"`
}

// Report fetches /web and /database-info and evaluates the site. It uses two
// requests, and the rules are pure so they can be tested without a network.
func (s *Service) Report(ctx context.Context, windowDays int) (Result, error) {
	if windowDays < 1 {
		return Result{}, apperr.Usage("window", "--window must be at least 1 day")
	}
	v, _, err := s.getJSON(ctx, api.Request{Path: "/web"})
	if err != nil {
		return Result{}, err
	}
	web, ok := v.(map[string]any)
	if !ok {
		return Result{}, apperr.New(apperr.CodeBadResponse, "/web did not return an object")
	}
	res := Result{}
	release := ""
	if info, _, err := s.getJSON(ctx, api.Request{Path: "/database-info", NoAuth: true}); err == nil {
		if m, ok := info.(map[string]any); ok {
			release, _ = m["release"].(string)
		}
	} else {
		res.Warnings = append(res.Warnings, "database-info unavailable: "+apperr.From(err).Message)
	}
	res.Data = Evaluate(web, release, windowDays, s.now())
	return res, nil
}

// Evaluate turns a decoded /web response into a Report.
func Evaluate(web map[string]any, release string, windowDays int, now time.Time) Report {
	r := Report{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		DataRelease: release,
		Totals:      map[string]int64{},
		Ratios:      map[string]float64{},
		Findings:    []Finding{},
	}
	docs, _ := web["documents"].(map[string]any)
	for k, v := range docs {
		if n, ok := categoryCount(v); ok {
			r.Totals[k] = n
		}
	}
	if cat, ok := docs["error"].(map[string]any); ok {
		if urls, ok := cat["urls"].([]any); ok {
			r.ErrorSample = urls[:min(len(urls), errorSampleLimit)]
		}
	}
	content, index, errs := r.Totals["content"], r.Totals["index"], r.Totals["error"]
	if content > 0 {
		r.Ratios["index_of_content"] = round(float64(index) / float64(content))
	}
	if content+errs > 0 {
		r.Ratios["error_of_crawled"] = round(float64(errs) / float64(content+errs))
	}

	add := func(f Finding) { r.Findings = append(r.Findings, f) }
	switch {
	case content > 0 && index == 0:
		add(Finding{"nothing_indexed", SeverityCritical,
			fmt.Sprintf("Seznam downloaded %d pages but none are in the index.", content), "index", 0})
	case content > 0 && r.Ratios["index_of_content"] < lowIndexRatio:
		add(Finding{"low_index_ratio", SeverityWarning,
			fmt.Sprintf("Only %.0f%% of downloaded pages are indexed.", r.Ratios["index_of_content"]*100),
			"index_of_content", r.Ratios["index_of_content"]})
	}
	if ratio, ok := r.Ratios["error_of_crawled"]; ok && ratio >= errorRatioWarn {
		sev := SeverityWarning
		if ratio >= errorRatioCrit {
			sev = SeverityCritical
		}
		add(Finding{"high_error_ratio", sev,
			fmt.Sprintf("%.0f%% of crawled pages are errors.", ratio*100), "error_of_crawled", ratio})
	}

	history, _ := web["history"].([]any)
	sortHistory(history)
	if len(history) < 2 {
		add(Finding{Code: "no_history", Severity: SeverityInfo, Message: "Not enough history to compute a trend."})
	} else {
		r.Trend = buildTrend(history, windowDays)
		evaluateTrend(r.Trend, add)
		if t, err := time.Parse("2006-01-02", r.Trend.To); err == nil {
			if age := now.Sub(t).Hours() / 24; age > staleDataDays+1 {
				add(Finding{"stale_history", SeverityWarning,
					fmt.Sprintf("Newest history row is %.0f days old.", age), "history_age_days", round(age)})
			}
		}
	}
	if release != "" {
		if t, err := time.Parse(time.RFC3339Nano, release); err == nil {
			if age := now.Sub(t).Hours() / 24; age > staleDataDays {
				add(Finding{"stale_release", SeverityWarning,
					fmt.Sprintf("Seznam's data release is %.1f days old.", age), "release_age_days", round(age)})
			}
		}
	}

	sort.SliceStable(r.Findings, func(i, j int) bool {
		return SeverityRank(r.Findings[i].Severity) > SeverityRank(r.Findings[j].Severity)
	})
	r.Severity = SeverityOK
	if len(r.Findings) > 0 {
		r.Severity = r.Findings[0].Severity
	}
	return r
}

func evaluateTrend(t *Trend, add func(Finding)) {
	if m, ok := t.Metrics["indexed"]; ok && m.Percent != nil && *m.Percent <= indexDropWarn {
		sev := SeverityWarning
		if *m.Percent <= indexDropCrit {
			sev = SeverityCritical
		}
		add(Finding{"index_drop", sev,
			fmt.Sprintf("Indexed pages fell %.1f%% (%d to %d) between %s and %s.", -*m.Percent, m.From, m.To, t.From, t.To),
			"indexed", *m.Percent})
	}
	if m, ok := t.Metrics["error"]; ok && m.Delta >= errorRiseMinAbs {
		// With no starting errors a percentage is undefined: count it as critical growth.
		if m.Percent == nil || *m.Percent >= errorRiseWarn {
			sev := SeverityWarning
			if m.Percent == nil || *m.Percent >= errorRiseCrit {
				sev = SeverityCritical
			}
			add(Finding{"error_rise", sev,
				fmt.Sprintf("Error pages rose by %d (%d to %d) between %s and %s.", m.Delta, m.From, m.To, t.From, t.To),
				"error", float64(m.Delta)})
		}
	}
}

// categoryCount accepts {count: n} and a bare number: the docs list doc_count
// as a category but the schema does not show its shape.
func categoryCount(v any) (int64, bool) {
	if m, ok := v.(map[string]any); ok {
		return api.Int(m["count"])
	}
	return api.Int(v)
}

func buildTrend(history []any, windowDays int) *Trend {
	latest := history[len(history)-1]
	toDate := historyDate(latest)
	base := history[0]
	if t, err := time.Parse("2006-01-02", toDate); err == nil {
		cutoff := t.AddDate(0, 0, -windowDays).Format("2006-01-02")
		for _, h := range history {
			if historyDate(h) <= cutoff {
				base = h
			}
		}
	}
	tr := &Trend{From: historyDate(base), To: toDate, Metrics: map[string]MetricChange{}}
	baseCounts, latestCounts := historyCounts(base), historyCounts(latest)
	for k, to := range latestCounts {
		from, ok := baseCounts[k]
		if !ok {
			continue
		}
		c := MetricChange{From: from, To: to, Delta: to - from}
		if from != 0 {
			p := round(float64(to-from) / float64(from) * 100)
			c.Percent = &p
		}
		tr.Metrics[k] = c
	}
	return tr
}

func historyCounts(v any) map[string]int64 {
	out := map[string]int64{}
	m, _ := v.(map[string]any)
	counts, _ := m["counts"].(map[string]any)
	for k, c := range counts {
		if n, ok := api.Int(c); ok {
			out[k] = n
		}
	}
	return out
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }
