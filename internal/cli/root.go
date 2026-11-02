// Package cli implements the szwm command tree.
package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/buildinfo"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/config"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/output"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/ratelimit"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

// Annotation keys read by `szwm schema`.
const (
	annEffect     = "szwm.effect"     // read | write | local
	annAuth       = "szwm.auth"       // required | optional | none
	annAPI        = "szwm.api"        // upstream operation
	annIdempotent = "szwm.idempotent" // true | false
	annRequests   = "szwm.requests"   // API calls per invocation
	annSummaryFor = "szwm.data"       // shape of envelope data
)

func meta(effect, auth, apiOp, idempotent, requests, data string) map[string]string {
	return map[string]string{
		annEffect: effect, annAuth: auth, annAPI: apiOp,
		annIdempotent: idempotent, annRequests: requests, annSummaryFor: data,
	}
}

type globals struct {
	profile  string
	keyFile  string
	output   string
	baseURL  string
	fields   []string
	raw      bool
	pretty   bool
	verbose  bool
	readOnly bool
	timeout  time.Duration
	deadline time.Duration
	retries  int
}

type app struct {
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	g       globals
	root    *cobra.Command
	w       *output.Writer
	svc     *wm.Service
	source  config.Source
	started time.Time
	exit    int

	stdinRead bool
	stdinData []byte
}

// outcome is what a command handler produces.
type outcome struct {
	res      wm.Result
	rows     []map[string]any
	columns  []string
	streamed bool
	// verbatim prints res.Raw as is, for commands whose output is not JSON.
	verbatim bool
}

type handler func(ctx context.Context, cmd *cobra.Command, args []string) (outcome, error)

// Run executes szwm and returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr, started: time.Now()}
	a.root = a.newRoot()
	a.root.SetArgs(args)
	a.root.SetOut(stdout)
	a.root.SetErr(stderr)
	if err := a.root.ExecuteContext(ctx); err != nil {
		// Handlers report their own errors, so only cobra parse errors land here.
		_ = a.setup()
		e := apperr.From(err)
		if e.Code == apperr.CodeInternal {
			e = apperr.New(apperr.CodeInvalidArgument, "%s", err.Error()).
				WithHint("Run `szwm schema` for the list of commands and flags.")
		}
		a.finish("", outcome{}, e)
	}
	return a.exit
}

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "szwm",
		Short: "Agent-first CLI for the Seznam Webmaster API (unofficial)",
		Long: `szwm inspects how Seznam.cz sees your site and requests reindexing.

Agents: start with 'szwm schema' (machine-readable contract) or 'szwm guide'.
Output is JSON whenever stdout is not a terminal, and szwm never prompts.`,
		Version:           buildinfo.Resolved(),
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: false},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		e := apperr.New(apperr.CodeInvalidArgument, "%s", err.Error())
		msg := err.Error()
		if strings.Contains(msg, "api-key") && !strings.Contains(msg, "api-key-file") {
			e.Hint = "szwm has no --api-key flag, because flags leak into process lists and logs. Use SZWM_API_KEY or --api-key-file."
		}
		return e
	})

	f := root.PersistentFlags()
	f.StringVar(&a.g.profile, "profile", "", "profile name or site to use (env SZWM_PROFILE)")
	f.StringVar(&a.g.keyFile, "api-key-file", "", "read the API key from this file, or - for stdin")
	f.StringVarP(&a.g.output, "output", "o", "", "json, ndjson, table or csv (default: table on a terminal, otherwise json; env SZWM_OUTPUT)")
	f.StringSliceVar(&a.g.fields, "fields", nil, "keep only these dotted paths in data, e.g. summary,items.url")
	f.BoolVar(&a.g.raw, "raw", false, "print the unmodified API response body instead of the envelope")
	f.BoolVar(&a.g.pretty, "pretty", false, "indent JSON output")
	f.BoolVar(&a.g.readOnly, "read-only", false, "refuse all writes (env SZWM_READ_ONLY=1)")
	f.DurationVar(&a.g.timeout, "timeout", 30*time.Second, "timeout per HTTP request")
	f.DurationVar(&a.g.deadline, "deadline", 0, "time limit for the whole command; batches stop cleanly and report not_attempted")
	f.IntVar(&a.g.retries, "retries", 3, "retries on rate limits and transient failures")
	f.StringVar(&a.g.baseURL, "base-url", "", "API base URL: official host or loopback only (env SZWM_BASE_URL)")
	f.BoolVar(&a.g.verbose, "verbose", false, "trace HTTP activity to stderr with the key redacted")

	root.AddCommand(
		a.webCmd(), a.documentsCmd(), a.historyCmd(), a.documentCmd(), a.reindexCmd(),
		a.databaseInfoCmd(), a.reportCmd(), a.quotaCmd(), a.apiCmd(),
		a.authCmd(), a.doctorCmd(), a.schemaCmd(), a.guideCmd(), a.mcpCmd(), a.versionCmd(),
	)
	return root
}

// setup prepares the writer; on a bad format it still installs JSON so that the
// resulting error is reported in a parseable way.
func (a *app) setup() error {
	if a.w != nil {
		return nil
	}
	a.w = &output.Writer{Format: output.JSON, Pretty: a.g.pretty, Fields: a.g.fields, Stdout: a.stdout, Stderr: a.stderr}
	name := a.g.output
	if name == "" {
		name = os.Getenv(config.EnvOutput)
	}
	if name == "" {
		if isTerminal(a.stdout) {
			a.w.Format = output.Table
		}
		return nil
	}
	f, err := output.ParseFormat(name)
	if err != nil {
		return err
	}
	a.w.Format = f
	return nil
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// wrap turns a handler into a cobra RunE that always emits a structured result.
func (a *app) wrap(h handler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		command := commandName(cmd)
		if err := a.setup(); err != nil {
			a.finish(command, outcome{}, err)
			return nil
		}
		ctx := cmd.Context()
		if a.g.deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, a.g.deadline)
			defer cancel()
		}
		o, err := h(ctx, cmd, args)
		a.finish(command, o, err)
		return nil
	}
}

func commandName(cmd *cobra.Command) string {
	parts := strings.Fields(cmd.CommandPath())
	if len(parts) > 0 {
		parts = parts[1:]
	}
	return strings.Join(parts, ".")
}

func (a *app) finish(command string, o outcome, err error) {
	env := output.Envelope{SchemaVersion: output.SchemaVersion, OK: true, Command: command, Data: o.res.Data}
	env.Meta = output.Meta{
		Version:         buildinfo.Resolved(),
		Profile:         a.source.Profile,
		Site:            a.source.Site,
		DurationMS:      time.Since(a.started).Milliseconds(),
		Truncated:       o.res.Truncated,
		Hints:           o.res.Hints,
		Warnings:        o.res.Warnings,
		UntrustedFields: o.res.Untrusted,
	}
	if a.svc != nil {
		stats := a.svc.API.Stats()
		env.Meta.Requests, env.Meta.Retries = stats.Requests, stats.Retries
	}
	fail := o.res.Err
	if err != nil {
		fail = apperr.From(err)
		env.Data = nil
	}
	if fail != nil {
		env.OK = false
		env.Error = fail
		a.exit = fail.ExitCode
	}
	view := output.View{Rows: o.rows, Columns: o.columns, Streamed: o.streamed}
	if a.g.raw || o.verbatim {
		view.Raw = o.res.Raw
	}
	a.w.Final(env, view)
}

// service builds the Service for the session's profile.
func (a *app) service() (*wm.Service, error) {
	if a.svc != nil {
		return a.svc, nil
	}
	svc, source, err := a.buildService(a.g.profile)
	if err != nil {
		return nil, err
	}
	a.svc, a.source = svc, source
	return svc, nil
}

func (a *app) buildService(profile string) (*wm.Service, config.Source, error) {
	res, err := config.Resolve(config.Input{KeyFile: a.g.keyFile, Profile: profile, Stdin: a.readStdin})
	if err != nil {
		return nil, config.Source{}, err
	}
	store := ratelimit.Store{Dir: config.StateDir()}
	id := config.Fingerprint(res.Key)
	client, err := a.newClient(res.Key, ratelimit.NewLimiter(store, id))
	if err != nil {
		return nil, config.Source{}, err
	}
	svc := &wm.Service{API: client, Ledger: ratelimit.NewLedger(store, id), ReadOnly: a.readOnly()}
	return svc, res.Source, nil
}

func (a *app) newClient(key string, limiter api.Limiter) (*api.Client, error) {
	base := a.g.baseURL
	if base == "" {
		base = os.Getenv(config.EnvBaseURL)
	}
	opts := api.Options{
		BaseURL: base, Key: key, Timeout: a.g.timeout, Retries: a.g.retries, Limiter: limiter,
		UserAgent: "szwm/" + buildinfo.Resolved() + " (+https://github.com/HonzaTuron/seznam-webmaster-cli)",
	}
	if a.g.verbose {
		opts.Trace = a.stderr
	}
	return api.New(opts)
}

func (a *app) readOnly() bool {
	if a.g.readOnly {
		return true
	}
	switch strings.ToLower(os.Getenv(config.EnvReadOnly)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// readStdin caches stdin because the key and the URL list can both come from it.
func (a *app) readStdin() ([]byte, error) {
	if a.stdinRead {
		return nil, apperr.Usage("stdin", "stdin was already consumed: it cannot supply both the API key and URLs")
	}
	a.stdinRead = true
	return io.ReadAll(a.stdin)
}
