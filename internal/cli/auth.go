package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/api"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/config"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

func (a *app) authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage API keys and per-site profiles",
		Long: `Manage per-site profiles. The API has no site parameter, because the key selects
the site, so keep one profile per site. Keys are stored in a 0600 config file and
are never printed; szwm shows a fingerprint so that keys can be compared.`,
	}
	cmd.AddCommand(a.authLoginCmd(), a.authStatusCmd(), a.authListCmd(), a.authUseCmd(), a.authLogoutCmd())
	return cmd
}

// verifyKey makes one authenticated call and returns the key's site, which is
// empty if the site has no data yet. Only a 401 counts as a bad key.
func (a *app) verifyKey(ctx context.Context, key string) (string, error) {
	client, err := a.newClient(key, nil)
	if err != nil {
		return "", err
	}
	site, err := wm.SiteOf(ctx, client)
	if apperr.IsCode(err, apperr.CodeForbidden) {
		return "", nil
	}
	return site, err
}

func (a *app) readLoginKey() (string, error) {
	if a.g.keyFile != "" {
		res, err := config.Resolve(config.Input{KeyFile: a.g.keyFile, Stdin: a.readStdin})
		return res.Key, err
	}
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stderr, "API key (input hidden): ")
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", apperr.New(apperr.CodeInternal, "cannot read key: %s", err.Error())
		}
		return strings.TrimSpace(string(data)), nil
	}
	data, err := a.readStdin()
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(data)), nil
}

func (a *app) authLoginCmd() *cobra.Command {
	var site string
	var noVerify, setDefault bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save an API key as a profile",
		Long: `Save a site's API key as a named profile. Find the key at
` + api.KeyURL + `. It comes from --api-key-file or stdin, so it never
appears in shell history. It is verified before saving, which also detects the
site, unless --no-verify is set.`,
		Example: `  printf %s "$KEY" | szwm auth login --profile shop
  szwm auth login --profile shop --api-key-file key.txt`,
		Args:        cobra.NoArgs,
		Annotations: meta("local", "none", "GET /web", "true", "1", "{profile, site, default, verified, key_fingerprint, config_path}"),
	}
	f := cmd.Flags()
	f.StringVar(&site, "site", "", "site domain, detected automatically when the key is verified; --profile also accepts it")
	f.BoolVar(&noVerify, "no-verify", false, "save without calling the API")
	f.BoolVar(&setDefault, "default", false, "make this the default profile")
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, _ []string) (outcome, error) {
		name := a.g.profile
		if name == "" {
			name = "default"
		}
		key, err := a.readLoginKey()
		if err != nil {
			return outcome{}, err
		}
		if key == "" {
			return outcome{}, apperr.Usage("api-key-file", "no API key was provided").
				WithHint("Pipe the key to stdin or pass --api-key-file.")
		}
		if !noVerify {
			detected, err := a.verifyKey(ctx, key)
			if err != nil {
				return outcome{}, err
			}
			if site == "" {
				site = detected
			}
		}
		cfg, err := config.Load()
		if err != nil {
			return outcome{}, err
		}
		cfg.Profiles[name] = config.Profile{Key: key, Site: site}
		if setDefault || cfg.DefaultProfile == "" {
			cfg.DefaultProfile = name
		}
		if err := cfg.Save(); err != nil {
			return outcome{}, apperr.New(apperr.CodeInternal, "cannot save config: %s", err.Error())
		}
		path, _ := config.Path()
		return outcome{res: wm.Result{Data: map[string]any{
			"profile": name, "site": site, "default": cfg.DefaultProfile == name,
			"verified": !noVerify, "key_fingerprint": config.Fingerprint(key), "config_path": path,
		}}}, nil
	})
	return cmd
}

func (a *app) authStatusCmd() *cobra.Command {
	var verify bool
	cmd := &cobra.Command{
		Use:         "status",
		Short:       "Show which key would be used, without revealing it",
		Example:     "  szwm auth status --verify --fields authenticated,source",
		Args:        cobra.NoArgs,
		Annotations: meta("local", "optional", "GET /web", "true", "0, or 1 with --verify", "{authenticated, key_fingerprint, source, verified, site}"),
	}
	cmd.Flags().BoolVar(&verify, "verify", false, "check the key against the API")
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, _ []string) (outcome, error) {
		res, err := config.Resolve(config.Input{KeyFile: a.g.keyFile, Profile: a.g.profile, Stdin: a.readStdin})
		if err != nil {
			return outcome{}, err
		}
		a.source = res.Source
		data := map[string]any{"authenticated": res.Key != "", "source": res.Source}
		if res.Key == "" {
			return outcome{res: wm.Result{Data: data, Err: api.MissingKeyError()}}, nil
		}
		data["key_fingerprint"] = config.Fingerprint(res.Key)
		if verify {
			site, err := a.verifyKey(ctx, res.Key)
			if err != nil {
				return outcome{}, err
			}
			data["verified"] = true
			if site != "" {
				data["site"] = site
			}
		}
		return outcome{res: wm.Result{Data: data}}, nil
	})
	return cmd
}

func (a *app) authListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List saved profiles",
		Args:        cobra.NoArgs,
		Annotations: meta("local", "none", "", "true", "0", "{name, site, default, key_fingerprint}[]"),
	}
	cmd.RunE = a.wrap(func(context.Context, *cobra.Command, []string) (outcome, error) {
		data, err := profileList()
		if err != nil {
			return outcome{}, err
		}
		return outcome{
			res: wm.Result{Data: data}, rows: data,
			columns: []string{"name", "site", "default", "key_fingerprint"},
		}, nil
	})
	return cmd
}

func profileList() ([]map[string]any, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(cfg.Profiles))
	for _, name := range cfg.Names() {
		p := cfg.Profiles[name]
		out = append(out, map[string]any{
			"name": name, "site": p.Site, "default": name == cfg.DefaultProfile,
			"key_fingerprint": config.Fingerprint(p.Key),
		})
	}
	return out, nil
}

func (a *app) authUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "use <profile>",
		Short:       "Set the default profile",
		Args:        cobra.ExactArgs(1),
		Annotations: meta("local", "none", "", "true", "0", "{default_profile}"),
	}
	cmd.RunE = a.wrap(func(_ context.Context, _ *cobra.Command, args []string) (outcome, error) {
		cfg, err := config.Load()
		if err != nil {
			return outcome{}, err
		}
		name, _, ok := cfg.Find(args[0])
		if !ok {
			return outcome{}, apperr.Usage("profile", "unknown profile or site %q", args[0])
		}
		cfg.DefaultProfile = name
		if err := cfg.Save(); err != nil {
			return outcome{}, apperr.New(apperr.CodeInternal, "cannot save config: %s", err.Error())
		}
		return outcome{res: wm.Result{Data: map[string]any{"default_profile": name}}}, nil
	})
	return cmd
}

func (a *app) authLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "logout",
		Short:       "Delete a saved profile",
		Example:     "  szwm auth logout --profile shop",
		Args:        cobra.NoArgs,
		Annotations: meta("local", "none", "", "true", "0", "{removed, default_profile}"),
	}
	cmd.RunE = a.wrap(func(context.Context, *cobra.Command, []string) (outcome, error) {
		if a.g.profile == "" {
			return outcome{}, apperr.Usage("profile", "pass --profile to choose which profile to delete")
		}
		cfg, err := config.Load()
		if err != nil {
			return outcome{}, err
		}
		name, _, ok := cfg.Find(a.g.profile)
		if !ok {
			return outcome{res: wm.Result{Data: map[string]any{"removed": false}}}, nil
		}
		delete(cfg.Profiles, name)
		if cfg.DefaultProfile == name {
			cfg.DefaultProfile = ""
		}
		if err := cfg.Save(); err != nil {
			return outcome{}, apperr.New(apperr.CodeInternal, "cannot save config: %s", err.Error())
		}
		return outcome{res: wm.Result{Data: map[string]any{"removed": true, "profile": name, "default_profile": cfg.DefaultProfile}}}, nil
	})
	return cmd
}
