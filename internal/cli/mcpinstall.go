package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/config"
	"github.com/HonzaTuron/seznam-webmaster-cli/internal/wm"
)

const defaultServerName = "seznam-webmaster"

// mcpInstallCmd registers this binary by its absolute path. Clients start MCP
// servers with their own PATH, and `go install` targets a directory that is
// often not on it, so `claude mcp add ... -- szwm` can fail to find the binary.
func (a *app) mcpInstallCmd() *cobra.Command {
	var name, scope string
	var allowWrites, printOnly, force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register this binary as an MCP server in Claude Code",
		Long: `Register szwm with Claude Code using the absolute path of this binary, so the
server starts whatever the client's PATH is. The server is read-only unless
--allow-writes is set. API keys are never written to the client's config: the
server reads your saved profiles when it starts.

For other clients, --print outputs the command and a JSON config to paste.`,
		Example: `  szwm mcp install
  szwm mcp install --allow-writes --force
  szwm mcp install --print --fields config`,
		Args:        cobra.NoArgs,
		Annotations: meta("local", "none", "", "false", "0", "{name, scope, command, args, read_only, registered, config}"),
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", defaultServerName, "server name in the client")
	f.StringVar(&scope, "scope", "user", "Claude Code scope: user, local or project")
	f.BoolVar(&allowWrites, "allow-writes", false, "expose the reindex tool")
	f.BoolVar(&printOnly, "print", false, "print the configuration without changing anything")
	f.BoolVar(&force, "force", false, "replace an existing server with this name")
	cmd.RunE = a.wrap(func(ctx context.Context, _ *cobra.Command, _ []string) (outcome, error) {
		switch scope {
		case "user", "local", "project":
		default:
			return outcome{}, apperr.Usage("scope", "--scope must be user, local or project")
		}
		res, err := config.Resolve(config.Input{Profile: a.g.profile})
		if err != nil {
			return outcome{}, err
		}
		exe, err := os.Executable()
		if err != nil {
			return outcome{}, apperr.New(apperr.CodeInternal, "cannot find this binary: %s", err.Error())
		}
		if exe, err = filepath.Abs(exe); err != nil {
			return outcome{}, apperr.New(apperr.CodeInternal, "cannot resolve this binary: %s", err.Error())
		}
		args := []string{"mcp"}
		readOnly := !allowWrites || a.readOnly()
		if readOnly {
			args = append(args, "--read-only")
		}
		if a.g.profile != "" {
			args = append(args, "--profile", a.g.profile)
		}
		data := map[string]any{
			"name": name, "scope": scope, "command": exe, "args": args,
			"read_only": readOnly, "registered": false,
			"config": map[string]any{"mcpServers": map[string]any{
				name: map[string]any{"command": exe, "args": args},
			}},
		}
		out := wm.Result{Data: data}
		switch res.Source.Kind {
		case "none":
			out.Warnings = append(out.Warnings, "No API key is saved, so tools will fail with auth_missing. Run `szwm auth login` for each site.")
		case "env":
			out.Warnings = append(out.Warnings, "The key came from the environment. The server sees it only if the client passes that variable on; `szwm auth login` is more reliable.")
		}
		if strings.HasPrefix(exe, os.TempDir()) {
			out.Warnings = append(out.Warnings, "This binary is in a temporary directory, so the registration will break when it is removed.")
		}
		if printOnly {
			return outcome{res: out}, nil
		}
		claude, err := exec.LookPath("claude")
		if err != nil {
			return outcome{}, apperr.Usage("client", "the claude command is not on PATH").
				WithHint("Use --print to get the configuration for another client.")
		}
		add := append([]string{"mcp", "add", "-s", scope, name, "--", exe}, args...)
		output, err := exec.CommandContext(ctx, claude, add...).CombinedOutput()
		if err != nil && strings.Contains(string(output), "already exists") {
			if !force {
				return outcome{}, apperr.Usage("name", "an MCP server named %q already exists in scope %s", name, scope).
					WithHint("Pass --force to replace it.")
			}
			if rmOut, rmErr := exec.CommandContext(ctx, claude, "mcp", "remove", "-s", scope, name).CombinedOutput(); rmErr != nil {
				return outcome{}, clientError(rmErr, rmOut)
			}
			output, err = exec.CommandContext(ctx, claude, add...).CombinedOutput()
		}
		if err != nil {
			return outcome{}, clientError(err, output)
		}
		data["registered"] = true
		out.Hints = append(out.Hints, "Verify with `claude mcp list`, then restart any running Claude Code session.")
		return outcome{res: out}, nil
	})
	return cmd
}

func clientError(err error, output []byte) error {
	msg := strings.TrimSpace(string(output))
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	if msg == "" {
		msg = err.Error()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return apperr.New(apperr.CodeInternal, "claude mcp failed: %s", msg)
	}
	return apperr.New(apperr.CodeInternal, "cannot run claude: %s", msg)
}
