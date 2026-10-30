package bridge

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default values applied when the operator leaves a field unset.
const (
	// DefaultAPIVersion is the prefix the two documented surfaces share:
	// Core serves /api/v1 (servers: /api/v1) and every entire-api cell does too
	// (servers: https://<cell>/api/v1).
	DefaultAPIVersion = "/api/v1"
	// DefaultPollInterval and DefaultPollTimeout bound PollRun when the
	// configuration names nothing. A run is not polled forever by accident.
	DefaultPollInterval = 10 * time.Second
	DefaultPollTimeout  = 30 * time.Minute
	// DefaultRequestTimeout bounds one individual HTTP exchange.
	DefaultRequestTimeout = 60 * time.Second

	// MaxInstructions is AgentReplace.instructions maxLength. It is a saved
	// configuration field limit, not a model context limit.
	MaxInstructions = 16384
	// MaxIdempotencyKeyLength is the Idempotency-Key maxLength on the run route.
	MaxIdempotencyKeyLength = 128
	// MaxAgentNameLength is AgentReplace.name maxLength.
	MaxAgentNameLength = 128
	// MaxAgentDescriptionLength is AgentReplace.description maxLength.
	MaxAgentDescriptionLength = 1024
	// MaxModelIDLength is AgentReplace.model maxLength.
	MaxModelIDLength = 128
	// MaxDescriptionLength is the shared free-form configuration description limit.
	MaxDescriptionLength = 1024
)

// Sentinel errors. Callers classify with errors.Is; every wrapped error keeps
// the chain, so a typed *HTTPError stays reachable through errors.As.
var (
	// ErrNotConfigured means the client was built without the configuration
	// needed to make the call (base URL, repository, agent, ...).
	ErrNotConfigured = errors.New("bridge: not configured")
	// ErrMissingBearer means no bearer token was supplied.
	ErrMissingBearer = errors.New("bridge: missing bearer token")
	// ErrInsecureEndpoint means an origin is not https.
	ErrInsecureEndpoint = errors.New("bridge: endpoint origin is not https")
	// ErrOriginNotAllowed means a request URL resolved outside the configured
	// origin allowlist. The request is never sent.
	ErrOriginNotAllowed = errors.New("bridge: origin not in allowlist")
	// ErrInvalidConfig means a value failed local validation before any
	// request was sent (an instruction block over 16384 characters, a missing
	// target, a missing etag, ...).
	ErrInvalidConfig = errors.New("bridge: invalid configuration")
	// ErrRunFailed means the run reached failed or timed_out.
	ErrRunFailed = errors.New("bridge: run failed")
	// ErrRunCanceled means the run ended canceled.
	ErrRunCanceled = errors.New("bridge: run canceled")
	// ErrOutputEmpty means the run succeeded but carried neither a signal
	// rationale nor output text.
	ErrOutputEmpty = errors.New("bridge: run produced no result text")
	// ErrOutputTruncated means the run succeeded but output.truncated is true,
	// so the returned text is a prefix rather than the whole output.
	ErrOutputTruncated = errors.New("bridge: run output truncated")
	// ErrContextCanceled means the caller's context ended while polling.
	ErrContextCanceled = errors.New("bridge: context canceled during run")
	// ErrPollTimeout means the local poll deadline passed first.
	ErrPollTimeout = errors.New("bridge: run poll timed out")
)

// Config is the injectable configuration. It is deliberately plain so a CPA
// plugin can build it from its YAML config block.
//
// The bearer is read from YAML key "token" (preferred) or "bearer"; it is
// copied into the client and never written back out. MarshalYAML on Config is
// not provided on purpose, so there is no round-trip that echoes it.
type Config struct {
	// CoreBaseURL serves /me/billing. Example host: https://us.console.entire.io
	// Core is prefixed with CoreAPIVersion.
	CoreBaseURL string `yaml:"core_base_url"`
	// CellBaseURL serves the project-scoped agent and run routes. Route
	// requests to the project's API URL returned by Core.
	CellBaseURL string `yaml:"cell_base_url"`
	// APIVersion is shared by both origins; defaults to DefaultAPIVersion.
	APIVersion string `yaml:"api_version,omitempty"`

	// ProjectHost is the project host slug (gh or et).
	ProjectHost string `yaml:"project_host"`
	// Project is the canonical public project name.
	Project string `yaml:"project"`

	// AllowedHosts is the extra origin allowlist, as host[:port] entries. The
	// origins of CoreBaseURL and CellBaseURL are always allowed. An entry
	// without a port matches any port on that host.
	AllowedHosts []string `yaml:"allowed_hosts,omitempty"`
	// AllowHTTP relaxes the https requirement. It exists for httptest servers
	// on 127.0.0.1 and must stay false against Entire.
	AllowHTTP bool `yaml:"allow_http,omitempty"`

	// Bearer is the Entire bearer token (Authorization: Bearer ...). It is the
	// one credential this package holds and it is never logged.
	Bearer string `yaml:"token,omitempty"`
	// BearerEnv names an environment variable to read the bearer from when
	// Bearer is empty, so the token can stay out of the plugin config file.
	BearerEnv string `yaml:"token_env,omitempty"`

	// AgentID is the agent whose configuration and runs are addressed.
	AgentID string `yaml:"agent_id,omitempty"`

	// TrailID and ChangeID make up RunTargetInput; both are required to start
	// a run. HeadSHA optionally pins the Change's head.
	TrailID  string `yaml:"trail_id,omitempty"`
	ChangeID string `yaml:"change_id,omitempty"`
	HeadSHA  string `yaml:"head_sha,omitempty"`

	// RequestTimeout bounds one HTTP exchange; PollInterval and PollTimeout
	// bound the whole run. Zero values take the Default* constants.
	RequestTimeout time.Duration `yaml:"request_timeout,omitempty"`
	PollInterval   time.Duration `yaml:"poll_interval,omitempty"`
	PollTimeout    time.Duration `yaml:"poll_timeout,omitempty"`

	// UserAgent overrides the default bridge user agent.
	UserAgent string `yaml:"user_agent,omitempty"`
}

// rawConfig mirrors Config with the bearer aliases kept separate, so both
// "token" and "bearer" work without a second struct.
type rawConfig struct {
	Config    `yaml:",inline"`
	BearerAlt string `yaml:"bearer,omitempty"`
}

// ParseConfig decodes a plugin configuration block and validates it. When the
// block is empty the documented defaults are applied; only a block that
// supplies something is strictly parsed, so a bare `enabled: true` still works.
func ParseConfig(raw []byte) (*Config, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &Config{}, nil
	}
	var rc rawConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return nil, fmt.Errorf("%w: decode yaml: %v", ErrInvalidConfig, err)
	}
	cfg := rc.Config
	if strings.TrimSpace(cfg.Bearer) == "" {
		cfg.Bearer = strings.TrimSpace(rc.BearerAlt)
	}
	return &cfg, nil
}

// String reports the configuration without the credential. Use it anywhere a
// config might land in a log line.
func (c *Config) String() string {
	if c == nil {
		return "Config<nil>"
	}
	token := "unset"
	if strings.TrimSpace(c.Bearer) != "" {
		token = "set"
	}
	return fmt.Sprintf("Config{core=%q cell=%q project=%s/%s agent=%q trail=%q change=%q token=%s}",
		c.CoreBaseURL, c.CellBaseURL, c.ProjectHost, c.Project, c.AgentID, c.TrailID, c.ChangeID, token)
}

// GoString keeps %#v from printing the bearer.
func (c *Config) GoString() string { return c.String() }

// Normalize applies defaults and trims the scalar fields. It does not
// validate; New does that when a private copy is kept.
func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.CoreBaseURL = strings.TrimRight(strings.TrimSpace(c.CoreBaseURL), "/")
	c.CellBaseURL = strings.TrimRight(strings.TrimSpace(c.CellBaseURL), "/")
	c.APIVersion = strings.TrimSpace(c.APIVersion)
	if c.APIVersion == "" {
		c.APIVersion = DefaultAPIVersion
	}
	if !strings.HasPrefix(c.APIVersion, "/") {
		c.APIVersion = "/" + c.APIVersion
	}
	c.APIVersion = strings.TrimRight(c.APIVersion, "/")
	c.ProjectHost = strings.TrimSpace(c.ProjectHost)
	c.Project = strings.TrimSpace(c.Project)
	c.AgentID = strings.TrimSpace(c.AgentID)
	c.TrailID = strings.TrimSpace(c.TrailID)
	c.ChangeID = strings.TrimSpace(c.ChangeID)
	c.HeadSHA = strings.TrimSpace(c.HeadSHA)
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = DefaultPollTimeout
	}
	hosts := make([]string, 0, len(c.AllowedHosts))
	for _, h := range c.AllowedHosts {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, strings.ToLower(h))
		}
	}
	c.AllowedHosts = hosts
}

// baseURL renders one origin, tolerating a base URL that already ends in the
// API version (the shape the OpenAPI servers entries publish).
func (c *Config) baseURL(origin string) (string, error) {
	if origin == "" {
		return "", fmt.Errorf("%w: missing base URL", ErrNotConfigured)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("%w: parse base URL %q: %v", ErrInvalidConfig, origin, err)
	}
	if u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http") {
		return origin, &PreflightError{Reason: ErrInsecureEndpoint, Detail: origin, Origin: u.Host}
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: base URL %q has no host", ErrInvalidConfig, origin)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: base URL %q must not carry a query or fragment", ErrInvalidConfig, origin)
	}
	base := strings.TrimRight(u.String(), "/")
	// url.Parse keeps the path in u.Path; compare against that, not the raw URL.
	path := strings.TrimRight(u.Path, "/")
	if path != "" && strings.HasSuffix(path, c.APIVersion) {
		return base, nil
	}
	return base + c.APIVersion, nil
}

// ProjectPrefix is /{host}/{project}, the prefix of every project route.
func (c *Config) ProjectPrefix() (string, error) {
	if c.ProjectHost == "" || c.Project == "" {
		return "", fmt.Errorf("%w: project_host and project are required", ErrNotConfigured)
	}
	return "/" + url.PathEscape(c.ProjectHost) + "/" + url.PathEscape(c.Project), nil
}
