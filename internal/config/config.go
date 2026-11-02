// Package config stores named profiles and resolves which API key to use.
//
// The saved Webmaster page lists several sites per account, while /web takes no
// site parameter: the key selects the site. Hence one profile per site.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HonzaTuron/seznam-webmaster-cli/internal/apperr"
)

// Environment variables.
const (
	EnvAPIKey    = "SZWM_API_KEY"
	EnvAPIKeyAlt = "SEZNAM_WEBMASTER_API_KEY"
	EnvProfile   = "SZWM_PROFILE"
	EnvConfigDir = "SZWM_CONFIG_DIR"
	EnvStateDir  = "SZWM_STATE_DIR"
	EnvBaseURL   = "SZWM_BASE_URL"
	EnvOutput    = "SZWM_OUTPUT"
	EnvReadOnly  = "SZWM_READ_ONLY"
)

// Profile is one site's credentials.
type Profile struct {
	Key  string `json:"key"`
	Site string `json:"site,omitempty"`
}

// File is the on-disk config.
type File struct {
	Version        int                `json:"version"`
	DefaultProfile string             `json:"default_profile,omitempty"`
	Profiles       map[string]Profile `json:"profiles"`
}

// Dir returns the config directory.
func Dir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "szwm"), nil
}

// StateDir returns the directory for rate-limit and quota state.
func StateDir() string {
	if d := os.Getenv(EnvStateDir); d != "" {
		return d
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "szwm")
	}
	return filepath.Join(os.TempDir(), "szwm")
}

// Path returns the config file path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config; a missing file yields an empty config.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	f := &File{Version: 1, Profiles: map[string]Profile{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, apperr.New(apperr.CodeInvalidArgument, "config file %s is not valid JSON: %s", path, err.Error())
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	return f, nil
}

// Save writes the config atomically with owner-only permissions.
func (f *File) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Find looks a profile up by name, then by site.
func (f *File) Find(ref string) (string, Profile, bool) {
	if p, ok := f.Profiles[ref]; ok {
		return ref, p, true
	}
	want := NormalizeSite(ref)
	for _, name := range f.Names() {
		if p := f.Profiles[name]; p.Site != "" && NormalizeSite(p.Site) == want {
			return name, p, true
		}
	}
	return "", Profile{}, false
}

// Names returns profile names sorted.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NormalizeSite strips scheme, www and trailing slash for comparison.
func NormalizeSite(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s = strings.TrimSuffix(s, "/")
	return strings.TrimPrefix(s, "www.")
}

// Source says where a key came from. It never contains the key.
type Source struct {
	Kind    string `json:"kind"` // key-file | profile | env | none
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	Site    string `json:"site,omitempty"`
}

// Resolved is the outcome of key resolution.
type Resolved struct {
	Key    string
	Source Source
}

// Fingerprint returns a stable non-reversible key identifier. It scopes local
// state and lets agents compare keys without ever seeing them.
func Fingerprint(key string) string {
	if key == "" {
		return "nokey"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// Input carries the explicit choices made on the command line.
type Input struct {
	KeyFile string
	Profile string
	Stdin   func() ([]byte, error)
}

// Resolve applies the precedence: --api-key-file, --profile, environment key,
// SZWM_PROFILE, default profile, the only profile. Finding nothing is not an
// error here, since many commands work without a key.
func Resolve(in Input) (Resolved, error) {
	if in.KeyFile != "" {
		key, err := readKeyFile(in.KeyFile, in.Stdin)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Key: key, Source: Source{Kind: "key-file"}}, nil
	}
	if in.Profile != "" {
		return fromProfile(in.Profile)
	}
	for _, name := range []string{EnvAPIKey, EnvAPIKeyAlt} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return Resolved{Key: v, Source: Source{Kind: "env", Name: name}}, nil
		}
	}
	if p := os.Getenv(EnvProfile); p != "" {
		return fromProfile(p)
	}
	f, err := Load()
	if err != nil {
		return Resolved{}, err
	}
	if f.DefaultProfile != "" {
		return fromProfile(f.DefaultProfile)
	}
	if len(f.Profiles) == 1 {
		return fromProfile(f.Names()[0])
	}
	if len(f.Profiles) > 1 {
		return Resolved{}, apperr.Usage("profile", "several profiles exist but none is the default: %s",
			strings.Join(f.Names(), ", ")).WithHint("Pass --profile <name> or run `szwm auth use <name>`.")
	}
	return Resolved{Source: Source{Kind: "none"}}, nil
}

func fromProfile(ref string) (Resolved, error) {
	f, err := Load()
	if err != nil {
		return Resolved{}, err
	}
	name, p, ok := f.Find(ref)
	if !ok {
		return Resolved{}, apperr.Usage("profile", "unknown profile or site %q; known profiles: %s",
			ref, strings.Join(f.Names(), ", ")).WithHint("Run `szwm auth login --profile <name>`.")
	}
	return Resolved{Key: p.Key, Source: Source{Kind: "profile", Profile: name, Site: p.Site}}, nil
}

func readKeyFile(path string, stdin func() ([]byte, error)) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		if stdin == nil {
			return "", apperr.Usage("api-key-file", "stdin is not available")
		}
		data, err = stdin()
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", apperr.Usage("api-key-file", "cannot read API key file: %s", err.Error())
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", apperr.Usage("api-key-file", "API key file is empty")
	}
	return key, nil
}
