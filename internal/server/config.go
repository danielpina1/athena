package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/sources/github"
)

// ConfigFile is the server's configuration in the state directory, written 0600 by `athena github setup`:
//
//	{"github": {"appId", "slug", "keyFile", "installationId", "repos": [], "intervalSeconds": 30, "receiveAll": []},
//	 "router": {"model": "haiku"}, "evalTimeoutSeconds": 600,
//	 "sandbox": {"enabled": true, "required": false, "allowedDomains": [], "allowWrite": [],
//	             "weakerNetworkIsolation": false, "caFile": ""}}
//
// It holds the path of the GitHub App's private key, never the key. Without the file, or without its github part,
// the server runs owner tasks only: no Source, no router.
const ConfigFile = "config.json"

// Defaults of the configuration.
const (
	DefaultRouterModel        = "haiku"
	DefaultEvalTimeoutSeconds = 600
	DefaultIntervalSeconds    = 30
)

// FileConfig is <stateDir>/config.json.
type FileConfig struct {
	GitHub             *GitHubConfig `json:"github,omitempty"`
	Router             RouterConfig  `json:"router"`
	EvalTimeoutSeconds int           `json:"evalTimeoutSeconds"`
	Sandbox            SandboxConfig `json:"sandbox"`
}

// SandboxConfig is Claude Code's sandbox for worker sessions (node.SandboxConfig). Every field is optional.
type SandboxConfig struct {
	// Enabled turns the sandbox on or off; unset means on where Claude Code has one (macOS and Linux).
	Enabled *bool `json:"enabled,omitempty"`
	// Required makes a worker fail to start when the sandbox cannot start, instead of running unsandboxed.
	Required bool `json:"required,omitempty"`
	// AllowedDomains are hosts workers' commands may reach besides GitHub ("*.example.com" for the subdomains).
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	// AllowWrite are absolute paths workers' commands may write besides the Ticket's directory.
	AllowWrite []string `json:"allowWrite,omitempty"`
	// WeakerNetworkIsolation (macOS) opens the trustd service, which gh needs behind a TLS-inspecting proxy.
	WeakerNetworkIsolation bool `json:"weakerNetworkIsolation,omitempty"`
	// CAFile is an absolute path to a PEM bundle workers' git and curl verify TLS with.
	CAFile string `json:"caFile,omitempty"`
}

// On reports whether the sandbox is on: Enabled, or unset on a platform that has it.
func (c SandboxConfig) On() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return node.SandboxSupported()
}

// nodeSandbox is the node's sandbox configuration, nil when it is off. The GitHub App's private key is denied to
// workers' commands: it is the one secret on this machine they could otherwise read.
func (c FileConfig) nodeSandbox() *node.SandboxConfig {
	sb := c.Sandbox
	if !sb.On() {
		return nil
	}
	var denyRead []string
	if c.GitHub != nil && c.GitHub.KeyFile != "" {
		denyRead = append(denyRead, c.GitHub.KeyFile)
	}
	return &node.SandboxConfig{
		Required:               sb.Required,
		AllowedDomains:         slices.Clone(sb.AllowedDomains),
		AllowWrite:             slices.Clone(sb.AllowWrite),
		DenyRead:               denyRead,
		WeakerNetworkIsolation: sb.WeakerNetworkIsolation,
		CAFile:                 sb.CAFile,
	}
}

var domainRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*$`)

func (c SandboxConfig) validate() error {
	for _, d := range c.AllowedDomains {
		if !domainRe.MatchString(d) {
			return fmt.Errorf("sandbox: allowedDomains entry %q is not a host name (example.com or *.example.com)", d)
		}
	}
	for _, p := range c.AllowWrite {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("sandbox: allowWrite entry %q must be an absolute path", p)
		}
	}
	if c.CAFile != "" && !filepath.IsAbs(c.CAFile) {
		return errors.New("sandbox: caFile must be an absolute path")
	}
	if c.Enabled != nil && *c.Enabled && !node.SandboxSupported() {
		return errors.New("sandbox: Claude Code has no sandbox on this platform; remove \"enabled\": true")
	}
	return nil
}

// GitHubConfig is the GitHub App the server works through and the repositories it polls.
type GitHubConfig struct {
	AppID          int64    `json:"appId"`
	Slug           string   `json:"slug"`
	KeyFile        string   `json:"keyFile"`
	InstallationID int64    `json:"installationId"`
	Repos          []string `json:"repos"`
	// IntervalSeconds is the time between two polls of every repository (default 30).
	IntervalSeconds int `json:"intervalSeconds"`
	// ReceiveAll lists the configured repositories that receive every message, not only mentions of the App, in every
	// scope (issues, pulls, comments, ci): the Policy's unaddressed admission cells and its ci row apply only to them.
	// Default empty.
	ReceiveAll []string `json:"receiveAll,omitempty"`
	// APIURL is GitHub's REST endpoint (default https://api.github.com); it is set for GitHub Enterprise or a test
	// stand-in, and left out of the file otherwise.
	APIURL string `json:"apiUrl,omitempty"`
}

// RouterConfig configures the router session.
type RouterConfig struct {
	// Model is the router's model (default haiku).
	Model string `json:"model"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// LoadConfig reads <stateDir>/config.json with its defaults filled in; a missing file is the default configuration.
// Unknown fields are refused, so a typo is not silently ignored.
func LoadConfig(stateDir string) (FileConfig, error) {
	path := filepath.Join(stateDir, ConfigFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileConfig{}.withDefaults(), nil
	}
	if err != nil {
		return FileConfig{}, fmt.Errorf("config: %w", err)
	}
	var c FileConfig
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return FileConfig{}, fmt.Errorf("config: %s: %w", path, err)
	}
	if c.GitHub != nil {
		if err := c.GitHub.validate(); err != nil {
			return FileConfig{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if c.EvalTimeoutSeconds < 0 {
		return FileConfig{}, fmt.Errorf("config: %s: evalTimeoutSeconds must not be negative", path)
	}
	if err := c.Sandbox.validate(); err != nil {
		return FileConfig{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return c.withDefaults(), nil
}

func (c FileConfig) withDefaults() FileConfig {
	if c.Router.Model == "" {
		c.Router.Model = DefaultRouterModel
	}
	if c.EvalTimeoutSeconds == 0 {
		c.EvalTimeoutSeconds = DefaultEvalTimeoutSeconds
	}
	if c.GitHub != nil {
		g := *c.GitHub
		if g.IntervalSeconds == 0 {
			g.IntervalSeconds = DefaultIntervalSeconds
		}
		g.Repos = slices.Clone(g.Repos)
		g.ReceiveAll = slices.Clone(g.ReceiveAll)
		c.GitHub = &g
	}
	return c
}

// EvalTimeout is how long an Eval may run after its candidate reached the router.
func (c FileConfig) EvalTimeout() time.Duration {
	return time.Duration(c.EvalTimeoutSeconds) * time.Second
}

// validate checks what the server needs to reach GitHub: the ids, the slug, an absolute key path and owner/name repos.
func (g *GitHubConfig) validate() error {
	switch {
	case g.AppID <= 0:
		return errors.New("github: appId must be a positive number")
	case strings.TrimSpace(g.Slug) == "" || strings.ContainsAny(g.Slug, "/[] \t\n"):
		return errors.New("github: slug must be the App's URL name")
	case g.InstallationID <= 0:
		return errors.New("github: installationId must be a positive number")
	case !filepath.IsAbs(g.KeyFile):
		return errors.New("github: keyFile must be an absolute path")
	case len(g.Repos) == 0:
		return errors.New("github: at least one repository (owner/name) is required")
	case g.IntervalSeconds < 0:
		return errors.New("github: intervalSeconds must not be negative")
	}
	seen := map[string]bool{}
	for _, r := range g.Repos {
		if !repoRe.MatchString(r) {
			return fmt.Errorf("github: repository %q is not owner/name", r)
		}
		if seen[strings.ToLower(r)] {
			return fmt.Errorf("github: repository %q is listed twice", r)
		}
		seen[strings.ToLower(r)] = true
	}
	all := map[string]bool{}
	for _, r := range g.ReceiveAll {
		if !seen[strings.ToLower(r)] {
			return fmt.Errorf("github: receiveAll repository %q is not one of repos", r)
		}
		if all[strings.ToLower(r)] {
			return fmt.Errorf("github: receiveAll repository %q is listed twice", r)
		}
		all[strings.ToLower(r)] = true
	}
	return nil
}

// appConfig is the github package's view of the App.
func (g *GitHubConfig) appConfig(version string) github.AppConfig {
	return github.AppConfig{AppID: g.AppID, Slug: g.Slug, KeyFile: g.KeyFile, InstallationID: g.InstallationID,
		APIURL: g.APIURL, Version: version}
}

// interval is the time between two polls.
func (g *GitHubConfig) interval() time.Duration {
	return time.Duration(g.IntervalSeconds) * time.Second
}

// repo returns the configured spelling of repo (compared case-insensitively) and whether it is configured.
func (g *GitHubConfig) repo(repo string) (string, bool) {
	for _, r := range g.Repos {
		if strings.EqualFold(r, repo) {
			return r, true
		}
	}
	return "", false
}

// SaveConfig writes <stateDir>/config.json, mode 0600, replacing the file in one rename.
func SaveConfig(stateDir string, c FileConfig) error {
	if err := ensurePrivateDir(stateDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	path := filepath.Join(stateDir, ConfigFile)
	f, err := os.CreateTemp(stateDir, ConfigFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("config: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// SetupGitHub records the GitHub App in <stateDir>/config.json, keeping the rest of the file. The key file must be a
// regular file that only its owner may read or write (0600 or narrower); its path is stored, made absolute, never the
// key itself. The server reads the configuration when it starts.
func SetupGitHub(stateDir string, g GitHubConfig) error {
	if g.KeyFile != "" {
		abs, err := filepath.Abs(g.KeyFile)
		if err != nil {
			return fmt.Errorf("github setup: key file: %w", err)
		}
		g.KeyFile = abs
	}
	if err := g.validate(); err != nil {
		return fmt.Errorf("github setup: %w", err)
	}
	if err := checkKeyFile(g.KeyFile); err != nil {
		return fmt.Errorf("github setup: %w", err)
	}
	c, err := LoadConfig(stateDir)
	if err != nil {
		return err
	}
	g.Repos = slices.Clone(g.Repos)
	g.ReceiveAll = slices.Clone(g.ReceiveAll)
	if g.IntervalSeconds == 0 {
		g.IntervalSeconds = DefaultIntervalSeconds
	}
	c.GitHub = &g
	return SaveConfig(stateDir, c)
}

// checkKeyFile refuses a key file that is missing, not a regular file, or open to group or others.
func checkKeyFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("key file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("key file %s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("key file %s has mode %04o: group and others must have no access (chmod 0600)", path, perm)
	}
	return nil
}

// GitHubCheck is what `athena github check` found: the App's bot, the installation's account and the repositories
// the installation reaches.
type GitHubCheck struct {
	Bot     string   `json:"bot"`
	BotID   int64    `json:"botId"`
	Account string   `json:"account"`
	Repos   []string `json:"repos"`
}

// CheckGitHub reads the configuration and checks that the server can work through the App: it mints an installation
// token, reads the bot account and the installation's account, and lists the repositories the installation reaches;
// every configured repository must be among them. hc may be nil (a client with a 30 s timeout).
func CheckGitHub(ctx context.Context, stateDir, version string, hc *http.Client) (GitHubCheck, error) {
	c, err := LoadConfig(stateDir)
	if err != nil {
		return GitHubCheck{}, err
	}
	if c.GitHub == nil {
		return GitHubCheck{}, fmt.Errorf("github is not configured: run athena github setup")
	}
	app, err := github.NewApp(c.GitHub.appConfig(version), hc, nil)
	if err != nil {
		return GitHubCheck{}, err
	}
	return checkApp(ctx, app, c.GitHub)
}

// checkApp is the check itself, on an App already made.
func checkApp(ctx context.Context, app *github.App, g *GitHubConfig) (GitHubCheck, error) {
	if _, err := app.InstallationToken(ctx); err != nil {
		return GitHubCheck{}, fmt.Errorf("github: mint an installation token: %w", err)
	}
	bot, botID, err := app.Bot(ctx)
	if err != nil {
		return GitHubCheck{}, fmt.Errorf("github: read the App's bot account: %w", err)
	}
	account, err := app.Account(ctx)
	if err != nil {
		return GitHubCheck{}, fmt.Errorf("github: read the installation's account: %w", err)
	}
	repos, err := app.Repos(ctx)
	if err != nil {
		return GitHubCheck{}, fmt.Errorf("github: list the installation's repositories: %w", err)
	}
	out := GitHubCheck{Bot: bot, BotID: botID, Account: account}
	for _, r := range repos {
		out.Repos = append(out.Repos, r.FullName)
	}
	slices.Sort(out.Repos)
	var missing []string
	for _, want := range g.Repos {
		if !slices.ContainsFunc(out.Repos, func(r string) bool { return strings.EqualFold(r, want) }) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("github: the installation does not reach %s (it reaches %s)", strings.Join(missing, ", "),
			listOrNone(out.Repos))
	}
	return out, nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "no repository"
	}
	return strings.Join(l, ", ")
}
