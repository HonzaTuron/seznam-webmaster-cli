package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/output"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/sitemap"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

const reindexPath = "/web/document/reindex"

// urlSource is the shared set of ways to pass URLs to a batch command.
type urlSource struct {
	stdin   bool
	file    string
	sitemap string
	maxURLs int
}

func (s *urlSource) addFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVar(&s.stdin, "stdin", false, "read URLs from stdin: one per line, or ndjson with a url field")
	f.StringVar(&s.file, "file", "", "read URLs from a file, in the same format as --stdin")
	f.StringVar(&s.sitemap, "sitemap", "", "read URLs from a sitemap or sitemap index URL (gzip supported)")
	f.IntVar(&s.maxURLs, "max-urls", 0, "process at most this many URLs; 0 for all")
}

func (s *urlSource) batchSource() bool {
	return s.stdin || s.file != "" || s.sitemap != ""
}

func (a *app) gatherURLs(ctx context.Context, args []string, s *urlSource) ([]string, bool, error) {
	if s.maxURLs < 0 {
		return nil, false, apperr.Usage("max-urls", "--max-urls must not be negative")
	}
	raw := make([]string, 0, len(args))
	useStdin := s.stdin
	for _, arg := range args {
		if arg == "-" {
			useStdin = true
			continue
		}
		raw = append(raw, arg)
	}
	if useStdin {
		data, err := a.readStdin()
		if err != nil {
			return nil, false, err
		}
		list, err := parseURLList(data)
		if err != nil {
			return nil, false, err
		}
		raw = append(raw, list...)
	}
	if s.file != "" {
		data, err := os.ReadFile(s.file)
		if err != nil {
			return nil, false, apperr.Usage("file", "cannot read URL file: %s", err.Error())
		}
		list, err := parseURLList(data)
		if err != nil {
			return nil, false, err
		}
		raw = append(raw, list...)
	}
	truncated := false
	if s.sitemap != "" {
		list, cut, err := sitemap.Fetch(ctx, s.sitemap, sitemap.Options{MaxURLs: s.maxURLs})
		if err != nil {
			return nil, false, err
		}
		truncated = cut
		raw = append(raw, list...)
	}
	urls, err := wm.CleanURLs(raw)
	if err != nil {
		return nil, false, err
	}
	if s.maxURLs > 0 && len(urls) > s.maxURLs {
		urls, truncated = urls[:s.maxURLs], true
	}
	return urls, truncated, nil
}

// parseURLList accepts plain lines and ndjson, so `szwm document -o ndjson`
// output can be filtered and piped straight into `szwm reindex --stdin`.
func parseURLList(data []byte) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "", strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "{"):
			var row struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				return nil, apperr.Usage("stdin", "invalid JSON line: %s", err.Error())
			}
			if row.URL != "" {
				out = append(out, row.URL)
			}
		default:
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, apperr.Usage("stdin", "cannot read URL list: %s", err.Error())
	}
	return out, nil
}

func (a *app) reindexCmd() *cobra.Command {
	var src urlSource
	var dryRun, yes, ignoreQuota bool
	cmd := &cobra.Command{
		Use:   "reindex [url...]",
		Short: "Ask Seznam to reindex pages (write operation)",
		Long: `Request reindexing of one or many pages.

This is the only write in the API. It needs admin or write rights on the site
and is limited to 500 requests per day. szwm keeps a local estimate of the quota,
cuts batches to it and reports the rest as not_attempted with a reason.

More than one URL requires --yes. There is no prompt: without --yes the command
fails with confirmation_required. Preview with --dry-run, which needs no key,
makes no API call and also works in read-only mode.

"submitted" means the request was accepted, not that the page was indexed.`,
		Example: `  szwm reindex https://example.com/new-page
  szwm reindex --sitemap https://example.com/sitemap.xml --dry-run
  szwm document --stdin --brief -o ndjson < urls.txt \
    | jq -c 'select(.type == "item" and .state == "not_indexed")' \
    | szwm reindex --stdin --yes`,
		Annotations: meta("write", "required", "POST /web/document/reindex", "true", "1 per URL", "ReindexData: dry_run, summary, items[], quota, estimated_seconds"),
	}
	src.addFlags(cmd)
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "show what would be submitted without calling the API")
	f.BoolVar(&yes, "yes", false, "confirm a write of more than one URL")
	f.BoolVar(&ignoreQuota, "ignore-quota", false, "submit even when the local quota estimate says the day is used up")
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, args []string) (outcome, error) {
		if a.readOnly() && !dryRun {
			return outcome{}, wm.ReadOnlyError()
		}
		urls, truncated, err := a.gatherURLs(ctx, args, &src)
		if err != nil {
			return outcome{}, err
		}
		if err := wm.CheckBulk(len(urls), yes, dryRun); err != nil {
			return outcome{}, err
		}
		svc, err := a.service()
		if err != nil {
			return outcome{}, err
		}
		if !dryRun && !svc.API.HasKey() {
			return outcome{}, api.MissingKeyError()
		}
		stream := a.w.Format == output.NDJSON
		res, err := svc.Reindex(ctx, wm.ReindexOptions{
			URLs: urls, DryRun: dryRun, IgnoreQuota: ignoreQuota,
			OnItem: func(item wm.ReindexItem) {
				if stream {
					a.w.Item(item)
				}
			},
		})
		if err != nil {
			return outcome{}, err
		}
		res.Truncated = res.Truncated || truncated
		data := res.Data.(wm.ReindexData)
		return outcome{
			res: res, rows: itemRows(data.Items), streamed: stream,
			columns: []string{"url", "status", "reason"},
		}, nil
	})
	return cmd
}

func (a *app) quotaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "quota",
		Short:       "Show the local estimate of today's reindex quota",
		Long:        "Show an estimate from this machine's ledger. The API has no quota endpoint, so requests sent from elsewhere are not counted.",
		Args:        cobra.NoArgs,
		Annotations: meta("local", "optional", "", "true", "0", "Quota: day, timezone, limit, used_estimate, remaining_estimate, estimated"),
	}
	cmd.RunE = a.wrap(func(context.Context, *cobra.Command, []string) (outcome, error) {
		svc, err := a.service()
		if err != nil {
			return outcome{}, err
		}
		return outcome{res: wm.Result{Data: svc.Ledger.Status()}}, nil
	})
	return cmd
}

func (a *app) apiCmd() *cobra.Command {
	var params []string
	var noAuth, yes bool
	cmd := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Call any API path directly, with auth, limits and retries handled",
		Long: `Escape hatch for endpoints without a dedicated command, such as ones Seznam
adds later. The path is relative to the API base. Anything except GET requires
--yes and is blocked in read-only mode.`,
		Example: `  szwm api GET /web/documents
  szwm api GET /web/document --param url=https://example.com/
  szwm api GET /__healthcheck --no-auth`,
		Args:        cobra.ExactArgs(2),
		Annotations: meta("read", "optional", "any", "false", "1", "Decoded response body"),
	}
	f := cmd.Flags()
	f.StringArrayVar(&params, "param", nil, "query parameter as name=value; repeatable")
	f.BoolVar(&noAuth, "no-auth", false, "do not send the API key")
	f.BoolVar(&yes, "yes", false, "confirm a non-GET request")
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, args []string) (outcome, error) {
		method := strings.ToUpper(args[0])
		if method != "GET" && method != "POST" {
			return outcome{}, apperr.Usage("method", "method must be GET or POST")
		}
		q := url.Values{}
		for _, p := range params {
			name, value, ok := strings.Cut(p, "=")
			if !ok || name == "" {
				return outcome{}, apperr.Usage("param", "--param %q is not name=value", p)
			}
			if name == "key" {
				return outcome{}, apperr.Usage("param", "the key parameter is added automatically")
			}
			q.Add(name, value)
		}
		if method != "GET" {
			if a.readOnly() {
				return outcome{}, wm.ReadOnlyError()
			}
			if !yes {
				return outcome{}, apperr.New(apperr.CodeConfirmationRequired, "%s needs --yes", method)
			}
		}
		svc, err := a.service()
		if err != nil {
			return outcome{}, err
		}
		resp, err := svc.API.Do(ctx, api.Request{Method: method, Path: args[1], Query: q, NoAuth: noAuth})
		if err != nil {
			return outcome{}, err
		}
		if method == "POST" && args[1] == reindexPath {
			_ = svc.Ledger.Add(1) // keep the estimate honest for direct calls
		}
		return outcome{res: wm.Result{Data: looseBody(resp), Raw: resp.Body}}, nil
	})
	return cmd
}

func looseBody(resp *api.Response) any {
	body := bytes.TrimSpace(resp.Body)
	if len(body) == 0 {
		return map[string]any{"status": resp.Status}
	}
	if v, err := api.Decode(body); err == nil {
		return v
	}
	return map[string]any{"status": resp.Status, "text": string(body)}
}
