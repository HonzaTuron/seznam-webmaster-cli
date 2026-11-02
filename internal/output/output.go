// Package output implements szwm's stdout contract.
//
// In json mode stdout carries exactly one JSON document per run, success or
// failure. In ndjson mode each item is one line and the last line is a summary.
// A failure also prints a one-line message to stderr, so it stays visible when
// stdout is piped into another tool.
package output

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

// SchemaVersion versions the envelope, error codes and exit codes.
const SchemaVersion = "1"

// Format is an output format.
type Format string

// Output formats.
const (
	JSON   Format = "json"
	NDJSON Format = "ndjson"
	Table  Format = "table"
	CSV    Format = "csv"
)

// ParseFormat validates a format name.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case JSON, NDJSON, Table, CSV:
		return f, nil
	}
	return "", apperr.Usage("output", "unknown output format %q (use json, ndjson, table or csv)", s)
}

// Meta describes how a result was produced.
type Meta struct {
	Version         string   `json:"szwm_version"`
	Profile         string   `json:"profile,omitempty"`
	Site            string   `json:"site,omitempty"`
	DurationMS      int64    `json:"duration_ms"`
	Requests        int64    `json:"requests"`
	Retries         int64    `json:"retries"`
	Truncated       bool     `json:"truncated,omitempty"`
	Hints           []string `json:"hints,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	UntrustedFields []string `json:"untrusted_fields,omitempty"`
}

// Envelope wraps every JSON result.
type Envelope struct {
	SchemaVersion string        `json:"schema_version"`
	OK            bool          `json:"ok"`
	Command       string        `json:"command"`
	Data          any           `json:"data,omitempty"`
	Error         *apperr.Error `json:"error,omitempty"`
	Meta          Meta          `json:"meta"`
}

// Writer renders results.
type Writer struct {
	Format Format
	Pretty bool
	Fields []string
	Stdout io.Writer
	Stderr io.Writer
}

// View carries the tabular form of a result next to its envelope.
type View struct {
	Rows     []map[string]any
	Columns  []string
	Raw      []byte
	Streamed bool
}

// Item writes one ndjson line immediately, so a long batch shows progress and
// survives being killed. In other formats it does nothing.
func (w *Writer) Item(item any) {
	if w.Format != NDJSON {
		return
	}
	m, ok := Normalize(item).(map[string]any)
	if !ok {
		m = map[string]any{"value": item}
	}
	w.writeLine(w.itemLine(m))
}

func (w *Writer) itemLine(m map[string]any) map[string]any {
	if len(w.Fields) > 0 {
		m, _ = Select(m, w.Fields).(map[string]any)
	}
	line := map[string]any{"type": "item"}
	for k, v := range m {
		line[k] = v
	}
	return line
}

func (w *Writer) writeLine(v any) {
	data, err := marshal(v, false)
	if err != nil {
		fmt.Fprintln(w.Stderr, "error: cannot encode output:", err)
		return
	}
	w.Stdout.Write(append(data, '\n'))
}

// Final writes the result and returns nothing: callers derive the exit code
// from env.Error.
func (w *Writer) Final(env Envelope, v View) {
	if len(v.Raw) > 0 && env.OK {
		w.Stdout.Write(v.Raw)
		if v.Raw[len(v.Raw)-1] != '\n' {
			io.WriteString(w.Stdout, "\n")
		}
		return
	}
	switch w.Format {
	case NDJSON:
		w.finalNDJSON(env, v)
	case Table:
		w.finalTabular(env, v, w.writeTable)
	case CSV:
		w.finalTabular(env, v, w.writeCSV)
	default:
		if len(w.Fields) > 0 && env.Data != nil {
			env.Data = Select(Normalize(env.Data), w.Fields)
		}
		data, err := marshal(env, w.Pretty)
		if err != nil {
			fmt.Fprintln(w.Stderr, "error: cannot encode output:", err)
			return
		}
		w.Stdout.Write(append(data, '\n'))
	}
	w.errorLine(env.Error)
}

func (w *Writer) finalNDJSON(env Envelope, v View) {
	if !v.Streamed {
		for _, row := range v.Rows {
			w.Item(row)
		}
		if v.Rows == nil && env.Data != nil {
			if arr, ok := Normalize(env.Data).([]any); ok {
				for _, e := range arr {
					w.Item(e)
				}
			} else {
				w.Item(env.Data)
			}
		}
	}
	line := map[string]any{
		"type": "summary", "schema_version": env.SchemaVersion, "ok": env.OK,
		"command": env.Command, "meta": env.Meta,
	}
	if env.Error != nil {
		line["error"] = env.Error
	}
	if summary := summaryOf(env.Data); summary != nil {
		line["summary"] = summary
	}
	w.writeLine(line)
}

func summaryOf(data any) any {
	if m, ok := Normalize(data).(map[string]any); ok {
		return m["summary"]
	}
	return nil
}

func (w *Writer) finalTabular(env Envelope, v View, render func([]string, [][]string)) {
	if env.Data == nil && v.Rows == nil {
		return
	}
	cols, rows := w.tabulate(env, v)
	if len(rows) > 0 {
		render(cols, rows)
	} else if env.OK {
		fmt.Fprintln(w.Stderr, "no rows")
	}
	if w.Format == Table {
		for _, h := range env.Meta.Hints {
			fmt.Fprintln(w.Stderr, "note:", h)
		}
		for _, h := range env.Meta.Warnings {
			fmt.Fprintln(w.Stderr, "warning:", h)
		}
	}
}

func (w *Writer) tabulate(env Envelope, v View) ([]string, [][]string) {
	maps := v.Rows
	cols := v.Columns
	if maps == nil {
		switch d := Normalize(env.Data).(type) {
		case []any:
			for _, e := range d {
				if m, ok := e.(map[string]any); ok {
					maps = append(maps, Flatten(m))
				}
			}
		case map[string]any:
			flat := Flatten(d)
			keys := sortedKeys(flat)
			if len(w.Fields) > 0 {
				keys = keys[:0]
				for _, f := range w.Fields {
					if _, ok := flat[f]; ok {
						keys = append(keys, f)
					}
				}
			}
			rows := make([][]string, 0, len(keys))
			for _, k := range keys {
				rows = append(rows, []string{k, stringify(flat[k])})
			}
			return []string{"FIELD", "VALUE"}, rows
		}
	}
	if len(w.Fields) > 0 {
		cols = w.Fields
	}
	if len(cols) == 0 {
		seen := map[string]bool{}
		for _, m := range maps {
			for k := range m {
				if !seen[k] {
					seen[k] = true
					cols = append(cols, k)
				}
			}
		}
		sort.Strings(cols)
	}
	rows := make([][]string, 0, len(maps))
	for _, m := range maps {
		flat := Flatten(m)
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = stringify(flat[c])
		}
		rows = append(rows, row)
	}
	return cols, rows
}

func (w *Writer) writeTable(cols []string, rows [][]string) {
	tw := tabwriter.NewWriter(w.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func (w *Writer) writeCSV(cols []string, rows [][]string) {
	cw := csv.NewWriter(w.Stdout)
	_ = cw.Write(cols)
	_ = cw.WriteAll(rows)
	cw.Flush()
}

func (w *Writer) errorLine(e *apperr.Error) {
	if e == nil {
		return
	}
	fmt.Fprintf(w.Stderr, "error: %s: %s\n", e.Code, e.Message)
	if e.Hint != "" {
		fmt.Fprintln(w.Stderr, "hint:", e.Hint)
	}
}

func marshal(v any, pretty bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// URLs are the main payload: keep & readable instead of &.
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Normalize converts any value to generic JSON types, keeping exact integers.
func Normalize(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return out
}

type pathTree map[string]pathTree

// Select keeps only the dotted paths in v. Arrays are mapped over, so
// items.url picks url from every element. Missing paths are simply absent.
func Select(v any, paths []string) any {
	tree := pathTree{}
	for _, p := range paths {
		node := tree
		for _, seg := range strings.Split(strings.TrimSpace(p), ".") {
			if node[seg] == nil {
				node[seg] = pathTree{}
			}
			node = node[seg]
		}
	}
	return selectTree(v, tree)
}

func selectTree(v any, tree pathTree) any {
	if len(tree) == 0 {
		return v
	}
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = selectTree(e, tree)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, child := range tree {
			if val, ok := t[k]; ok {
				out[k] = selectTree(val, child)
			}
		}
		return out
	}
	return v
}

// Flatten turns nested objects into dotted keys. Arrays stay as values.
func Flatten(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if nested, ok := v.(map[string]any); ok {
			for k, val := range nested {
				walk(prefix+"."+k, val)
			}
			return
		}
		out[prefix] = v
	}
	for k, v := range m {
		walk(k, v)
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case []any:
		return fmt.Sprintf("[%d items]", len(t))
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}
