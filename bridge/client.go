package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// headerAuthorization and headerIdempotencyKey are the two request headers
	// this client sets on its own.
	headerAuthorization = "Authorization"
	headerIdempotency   = "Idempotency-Key"
	headerIfMatch       = "If-Match"

	maxResponseBytes = 8 << 20 // hard cap on any decoded response body
)

// Transport is the http.RoundTripper the client dials through. Any
// http.RoundTripper satisfies it, so a test can pass httptest.Server.Client()
// while production passes the host's instrumented transport.
type Transport = http.RoundTripper

// Client is a bridge bound to one account, project and agent. It is safe for
// concurrent use: every field is fixed at construction except the per-run
// inputs passed to each call.
type Client struct {
	cfg       *Config   // private copy; the caller's struct is not retained
	bearer    string    // never rendered by any method
	transport Transport // injected
	http      *http.Client
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	// lastETag caches the JSON etag from the most recent read or write so a
	// caller can replace without re-reading.
	lastETag string
}

// New builds a client. transport may be nil, in which case a plain
// http.Client is used; AllowHTTP must be set for an http origin (httptest).
// origin allowlisting is derived from the config plus cfg.AllowedHosts.
func New(cfg *Config, transport Transport) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: nil configuration", ErrNotConfigured)
	}
	c := *cfg // copy: the client never aliases caller memory
	c.Normalize()

	bearer := strings.TrimSpace(c.Bearer)
	if bearer == "" && c.BearerEnv != "" {
		bearer = strings.TrimSpace(os.Getenv(c.BearerEnv))
		if bearer == "" {
			return nil, fmt.Errorf("%w: environment variable %s is empty", ErrMissingBearer, c.BearerEnv)
		}
	}
	if bearer == "" {
		return nil, ErrMissingBearer
	}
	// Drop the credential from the retained copy; only c.bearer carries it.
	c.Bearer = ""

	if transport == nil {
		transport = http.DefaultTransport
	}
	cl := &Client{
		cfg:       &c,
		bearer:    bearer,
		transport: transport,
		http: &http.Client{
			Transport: transport,
			Timeout:   c.RequestTimeout,
			// Never follow a redirect: the bearer must not travel to an origin
			// that the allowlist has not approved.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:   time.Now,
		sleep: sleepCtx,
	}
	// Fail fast on a malformed or insecure base URL rather than at first call.
	if c.CoreBaseURL != "" {
		if _, err := cl.coreURL(""); err != nil {
			return nil, err
		}
	}
	if c.CellBaseURL != "" {
		if _, err := cl.cellURL(""); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Config reports a copy of the effective configuration, bearer removed.
func (c *Client) Config() Config {
	if c == nil || c.cfg == nil {
		return Config{}
	}
	out := *c.cfg
	out.Bearer = ""
	return out
}

// SetAgent switches which agent subsequent ReadAgent / ReplaceAgent calls
// address.
func (c *Client) SetAgent(agentID string) {
	if c == nil || c.cfg == nil {
		return
	}
	c.cfg.AgentID = strings.TrimSpace(agentID)
}

// SetTarget switches the Trail/Change a subsequent StartRun addresses.
func (c *Client) SetTarget(trailID, changeID, headSHA string) {
	if c == nil || c.cfg == nil {
		return
	}
	c.cfg.TrailID = strings.TrimSpace(trailID)
	c.cfg.ChangeID = strings.TrimSpace(changeID)
	c.cfg.HeadSHA = strings.TrimSpace(headSHA)
}

// LastETag returns the JSON etag most recently observed.
func (c *Client) LastETag() string {
	if c == nil {
		return ""
	}
	return c.lastETag
}

// SetTransport swaps the transport behind subsequent requests. It exists so a
// host can attach an instrumented RoundTripper after construction (for example
// one that writes request-log entries) without losing the client's state.
func (c *Client) SetTransport(t Transport) {
	if c == nil {
		return
	}
	if t == nil {
		t = http.DefaultTransport
	}
	c.transport = t
	c.http.Transport = t
}

// ---------------------------------------------------------------------------
// URL construction and the origin allowlist
// ---------------------------------------------------------------------------

// coreURL and cellURL resolve a path against the configured origin, prepend
// the API version and then check the result against the allowlist.
func (c *Client) coreURL(path string) (string, error) {
	base, err := c.cfg.baseURL(c.cfg.CoreBaseURL)
	if err != nil {
		return "", err
	}
	return c.joinAndCheck(base, path)
}

func (c *Client) cellURL(path string) (string, error) {
	base, err := c.cfg.baseURL(c.cfg.CellBaseURL)
	if err != nil {
		return "", err
	}
	return c.joinAndCheck(base, path)
}

// coreURLTyped is coreURL with a caller-supplied query string kept separate
// from the path, so the path checks are not confused by a "?" or "&".
func (c *Client) coreURLTyped(path, query string) (string, error) {
	base, err := c.cfg.baseURL(c.cfg.CoreBaseURL)
	if err != nil {
		return "", err
	}
	u, err := c.joinAndCheck(base, path)
	if err != nil {
		return "", err
	}
	if query == "" {
		return u, nil
	}
	// The query is built by this package from caller-supplied values that are
	// already escaped; a bare fragment character is refused above.
	return u + "?" + strings.TrimPrefix(query, "?"), nil
}

func (c *Client) joinAndCheck(base, path string) (string, error) {
	if path == "" {
		// Validation-only call from New.
		u, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("%w: parse %q: %v", ErrInvalidConfig, base, err)
		}
		if err := c.checkOrigin(u); err != nil {
			return "", err
		}
		return base, nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: route path %q must start with /", ErrInvalidConfig, path)
	}
	if strings.Contains(path, "#") {
		return "", fmt.Errorf("%w: route path %q must not carry a fragment", ErrInvalidConfig, path)
	}
	full := base + path
	u, err := url.Parse(full)
	if err != nil {
		return "", fmt.Errorf("%w: parse %q: %v", ErrInvalidConfig, full, err)
	}
	// The only permitted userinfo is none at all: a token pasted into the URL
	// must never travel, and %40 in a path is not parsed as host userinfo.
	if u.User != nil || u.Host == "" {
		return "", fmt.Errorf("%w: route %q must not carry userinfo", ErrInvalidConfig, path)
	}
	if !strings.HasPrefix(u.Path, pathOfBase(base)) {
		return "", fmt.Errorf("%w: route path %q escapes the base path", ErrInvalidConfig, path)
	}
	if err := c.checkOrigin(u); err != nil {
		return "", err
	}
	return full, nil
}

func pathOfBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	return strings.TrimRight(u.Path, "/") + "/"
}

// checkOrigin enforces the allowlist. The rule is host[:port] equality against
// the configured origins plus every cfg.AllowedHosts entry; an entry without a
// port matches any port on that host. An empty allowlist is not a wildcard.
func (c *Client) checkOrigin(u *url.URL) error {
	if u.Scheme != "https" && !(c.cfg.AllowHTTP && u.Scheme == "http") {
		return &PreflightError{Reason: ErrInsecureEndpoint, Detail: u.Redacted(), Origin: u.Host}
	}
	if u.Host == "" {
		return &PreflightError{Reason: ErrOriginNotAllowed, Detail: u.Redacted(), Origin: u.Host}
	}
	wantHost := strings.ToLower(u.Host)
	entries := c.allowedOrigins()
	for _, e := range entries {
		if strings.EqualFold(e, wantHost) {
			return nil
		}
		// An entry without a port matches that host on any port.
		if host, _, hasPort := splitHostPort(e); !hasPort && strings.EqualFold(host, hostOnly(wantHost)) {
			return nil
		}
	}
	return &PreflightError{
		Reason: ErrOriginNotAllowed,
		Detail: fmt.Sprintf("%s is not in the allowlist %v", u.Redacted(), entries),
		Origin: u.Host,
	}
}

// allowedOrigins lists host[:port] entries: the two base URLs first, then the
// configured extras, deduplicated and sorted for a stable error message.
func (c *Client) allowedOrigins() []string {
	seen := make(map[string]struct{})
	add := func(raw string) {
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return
		}
		seen[strings.ToLower(u.Host)] = struct{}{}
	}
	add(c.cfg.CoreBaseURL)
	add(c.cfg.CellBaseURL)
	for _, h := range c.cfg.AllowedHosts {
		seen[strings.ToLower(h)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// escapeSegment percent-encodes one path segment. It is stricter than
// url.PathEscape on purpose: "." and ".." survive PathEscape unchanged, so an
// id of ".." would climb a path level. Here "." and ".." become "%2E" and
// "%2E%2E", and a "/" becomes "%2F", so an id can only ever address itself.
func escapeSegment(s string) string {
	if s == "." || s == ".." {
		return strings.Repeat("%2E", len(s))
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~', ch == ':', ch == '@':
			b.WriteByte(ch)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex2(ch)))
		}
	}
	return b.String()
}

const hexDigits = "0123456789ABCDEF"

func hex2(b byte) string {
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0f]})
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 {
		return h[:i]
	}
	return h
}

func splitHostPort(h string) (host, port string, hasPort bool) {
	i := strings.LastIndex(h, ":")
	if i < 0 {
		return h, "", false
	}
	return h[:i], h[i+1:], true
}

// ---------------------------------------------------------------------------
// request plumbing
// ---------------------------------------------------------------------------

type requestOptions struct {
	method         string
	url            string
	body           any
	ifMatch        string
	idempotencyKey string
	accept         string
}

// do performs one exchange, returning the decoded body on 2xx and an
// *HTTPError otherwise. The bearer is attached here and nowhere else.
func (c *Client) do(ctx context.Context, opts requestOptions) ([]byte, *http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var body io.Reader
	if opts.body != nil {
		raw, err := json.Marshal(opts.body)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge: encode %s body: %w", opts.method, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, opts.method, opts.url, body)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: build %s request: %v", ErrInvalidConfig, opts.method, err)
	}
	req.Header.Set(headerAuthorization, "Bearer "+c.bearer)
	req.Header.Set("Accept", opts.accept)
	if opts.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if opts.ifMatch != "" {
		req.Header.Set(headerIfMatch, opts.ifMatch)
	}
	if opts.idempotencyKey != "" {
		req.Header.Set(headerIdempotency, opts.idempotencyKey)
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.http.Do(req)
	if err != nil {
		// net/http errors quote the URL only; they never carry the header set.
		return nil, nil, fmt.Errorf("bridge: %s %s: %w", opts.method, opts.url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp, fmt.Errorf("bridge: %s %s: read body: %w", opts.method, opts.url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp, parseProblem(resp, opts.method, opts.url, raw)
	}
	return raw, resp, nil
}

func (c *Client) userAgent() string {
	if c.cfg.UserAgent != "" {
		return c.cfg.UserAgent
	}
	return "cpa-entire-plugin-bridge/0.1 (+agent-task-bridge; not a model gateway)"
}

// decodeInto unmarshals with a typed error so callers can report exactly which
// response shape failed.
func decodeInto(method, url, what string, raw []byte, dst any) error {
	if len(raw) == 0 {
		return &DecodeError{Method: method, URL: url, What: what, Err: io.ErrUnexpectedEOF}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &DecodeError{Method: method, URL: url, What: what, Err: err}
	}
	return nil
}
