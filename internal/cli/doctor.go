package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	seznamwebmaster "github.com/HonzaTuron/seznam-webmaster-cli"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/config"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | warn | fail | skip
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func (a *app) doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose config, key, connectivity and API contract drift",
		Long: `Run every check and report each with a concrete fix. Exit 0 means no check
failed; warnings do not fail. spec_drift compares the live swagger.json with the
snapshot szwm was built against, so upstream API changes surface early.`,
		Example:     "  szwm doctor --fields checks.name,checks.status,checks.fix",
		Args:        cobra.NoArgs,
		Annotations: meta("read", "optional", "GET /__healthcheck, GET /database-info, GET /web, GET /swagger.json", "true", "4", "{checks[]: {name, status, detail, fix}}"),
	}
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, _ []string) (outcome, error) {
		checks := a.runChecks(ctx)
		res := wm.Result{Data: map[string]any{"checks": checks}}
		for _, c := range checks {
			if c.Status == "fail" {
				res.Err = apperr.New(apperr.CodeDoctorFailed, "check %s failed: %s", c.Name, c.Detail).WithHint(c.Fix)
				break
			}
		}
		return outcome{res: res, rows: itemRows(checks), columns: []string{"name", "status", "detail", "fix"}}, nil
	})
	return cmd
}

func (a *app) runChecks(ctx context.Context) []check {
	var checks []check
	add := func(c check) { checks = append(checks, c) }
	add(checkConfigFile())
	add(checkStateDir())

	svc, err := a.service()
	if err != nil {
		add(check{Name: "credentials", Status: "fail", Detail: apperr.From(err).Message, Fix: apperr.From(err).Hint})
		return checks
	}
	add(apiCheck(ctx, svc, "api_health", api.Request{Method: "GET", Path: "/__healthcheck", NoAuth: true}))
	add(checkRelease(ctx, svc))

	if svc.API.HasKey() {
		add(check{Name: "api_key_present", Status: "pass", Detail: "source " + a.source.Kind})
		add(checkKey(ctx, svc))
	} else {
		fix := api.MissingKeyError().Hint
		add(check{Name: "api_key_present", Status: "fail", Detail: "no key resolved", Fix: fix})
		add(check{Name: "api_key_valid", Status: "skip", Detail: "no key"})
	}
	add(checkSpecDrift(ctx, svc))
	return checks
}

func checkConfigFile() check {
	path, err := config.Path()
	if err != nil {
		return check{Name: "config_file", Status: "warn", Detail: err.Error()}
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return check{Name: "config_file", Status: "pass", Detail: "no config file; environment keys only"}
	}
	if err != nil {
		return check{Name: "config_file", Status: "warn", Detail: err.Error()}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return check{
			Name: "config_file", Status: "fail",
			Detail: fmt.Sprintf("%s is readable by other users (%o)", path, info.Mode().Perm()),
			Fix:    fmt.Sprintf("chmod 600 %q", path),
		}
	}
	return check{Name: "config_file", Status: "pass", Detail: path}
}

func checkStateDir() check {
	dir := config.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return stateWarning(dir, err)
	}
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return stateWarning(dir, err)
	}
	os.Remove(probe)
	return check{Name: "state_dir", Status: "pass", Detail: dir}
}

func stateWarning(dir string, err error) check {
	return check{
		Name: "state_dir", Status: "warn",
		Detail: "cross-process rate limiting and the quota ledger are off: " + err.Error(),
		Fix:    "Point " + config.EnvStateDir + " at a writable directory instead of " + dir + ".",
	}
}

func apiCheck(ctx context.Context, svc *wm.Service, name string, req api.Request) check {
	if _, err := svc.API.Do(ctx, req); err != nil {
		e := apperr.From(err)
		return check{Name: name, Status: "fail", Detail: e.Message, Fix: "Check network access to reporter.seznam.cz."}
	}
	return check{Name: name, Status: "pass"}
}

func checkRelease(ctx context.Context, svc *wm.Service) check {
	info, err := svc.DatabaseInfo(ctx)
	if err != nil {
		return check{Name: "database_release", Status: "fail", Detail: apperr.From(err).Message, Fix: "Check network access to reporter.seznam.cz."}
	}
	c := check{Name: "database_release", Status: "pass"}
	if m, ok := info.Data.(map[string]any); ok {
		c.Detail, _ = m["release"].(string)
	}
	return c
}

func checkKey(ctx context.Context, svc *wm.Service) check {
	site, err := svc.Site(ctx)
	if err == nil {
		if site == "" {
			return check{Name: "api_key_valid", Status: "pass", Detail: "key accepted; site has no data yet"}
		}
		return check{Name: "api_key_valid", Status: "pass", Detail: "site " + site}
	}
	e := apperr.From(err)
	if e.Code == apperr.CodeAuthInvalid {
		return check{Name: "api_key_valid", Status: "fail", Detail: e.Message, Fix: e.Hint}
	}
	return check{Name: "api_key_valid", Status: "warn", Detail: e.Message}
}

func checkSpecDrift(ctx context.Context, svc *wm.Service) check {
	resp, err := svc.API.Do(ctx, api.Request{Method: "GET", Path: "/swagger.json", NoAuth: true})
	if err != nil {
		return check{Name: "spec_drift", Status: "warn", Detail: "cannot fetch live spec: " + apperr.From(err).Message}
	}
	diff, err := SpecDiff(seznamwebmaster.SwaggerSnapshot, resp.Body)
	if err != nil {
		return check{Name: "spec_drift", Status: "warn", Detail: err.Error()}
	}
	if len(diff) > 0 {
		return check{
			Name: "spec_drift", Status: "warn", Detail: strings.Join(diff, "; "),
			Fix: "The API changed: update szwm. `szwm api` can reach new endpoints meanwhile.",
		}
	}
	return check{Name: "spec_drift", Status: "pass", Detail: "live spec matches the built-in snapshot"}
}

type specDoc struct {
	Paths map[string]map[string]struct {
		Parameters []struct {
			Name string `json:"name"`
			Ref  string `json:"$ref"`
		} `json:"parameters"`
	} `json:"paths"`
}

// SpecDiff lists operations and parameters that differ between two specs.
func SpecDiff(snapshot, live []byte) ([]string, error) {
	a, err := operations(snapshot)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	b, err := operations(live)
	if err != nil {
		return nil, fmt.Errorf("live spec: %w", err)
	}
	var diff []string
	for op, params := range a {
		other, ok := b[op]
		switch {
		case !ok:
			diff = append(diff, "removed "+op)
		case params != other:
			diff = append(diff, fmt.Sprintf("changed %s: parameters [%s] became [%s]", op, params, other))
		}
	}
	for op := range b {
		if _, ok := a[op]; !ok {
			diff = append(diff, "added "+op)
		}
	}
	sort.Strings(diff)
	return diff, nil
}

func operations(data []byte) (map[string]string, error) {
	var doc specDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	ops := map[string]string{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			names := make([]string, 0, len(op.Parameters))
			for _, p := range op.Parameters {
				if p.Name != "" {
					names = append(names, p.Name)
				} else {
					names = append(names, strings.TrimPrefix(p.Ref, "#/parameters/"))
				}
			}
			sort.Strings(names)
			ops[strings.ToUpper(method)+" "+path] = strings.Join(names, ",")
		}
	}
	return ops, nil
}
