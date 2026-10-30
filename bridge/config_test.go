package bridge_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/yg/cpa-entire-plugin/bridge"
)

func TestNewRequiresBearer(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{CoreBaseURL: "https://core.example", CellBaseURL: "https://cell.example"}
	_, err := bridge.New(cfg, nil)
	if !errors.Is(err, bridge.ErrMissingBearer) {
		t.Fatalf("want ErrMissingBearer, got %v", err)
	}
}

func TestNewReadsBearerFromNamedEnvironment(t *testing.T) {
	// Not parallel: the child process environment is shared.
	const envName = "BRIDGE_TEST_TOKEN_VAR"
	t.Setenv(envName, testToken)
	cfg := &bridge.Config{
		CoreBaseURL: "https://core.example",
		CellBaseURL: "https://cell.example",
		BearerEnv:   envName,
	}
	client, err := bridge.New(cfg, nil)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	// The client must not hand the credential back through Config().
	got := client.Config()
	if strings.Contains(got.String(), testToken) {
		t.Fatal("Client.Config() exposed the bearer")
	}
	if got.Bearer != "" {
		t.Fatal("Client.Config().Bearer is not empty")
	}
	if os.Getenv(envName) != testToken {
		t.Fatal("environment was mutated")
	}
}

func TestNewRejectsEmptyNamedEnvironment(t *testing.T) {
	const envName = "BRIDGE_TEST_TOKEN_ABSENT"
	t.Setenv(envName, "")
	cfg := &bridge.Config{CoreBaseURL: "https://core.example", CellBaseURL: "https://cell.example", BearerEnv: envName}
	if _, err := bridge.New(cfg, nil); !errors.Is(err, bridge.ErrMissingBearer) {
		t.Fatalf("want ErrMissingBearer, got %v", err)
	}
}

func TestParseConfigBearerAliasesAndDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"token key", "token: from-token\ncell_base_url: https://cell.example\n", "from-token"},
		{"bearer alias", "bearer: from-bearer\ncell_base_url: https://cell.example\n", "from-bearer"},
		{"token wins", "token: from-token\nbearer: from-bearer\n", "from-token"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := bridge.ParseConfig([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg.Bearer != tc.want {
				t.Fatalf("bearer = %q, want %q", cfg.Bearer, tc.want)
			}
		})
	}

	empty, err := bridge.ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(empty): %v", err)
	}
	if empty == nil {
		t.Fatal("ParseConfig(empty) returned nil config")
	}

	if _, err := bridge.ParseConfig([]byte("token: [unclosed\n")); !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig on bad yaml, got %v", err)
	}
}

func TestConfigNormalizeAppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{CellBaseURL: "https://cell.example/", APIVersion: "api/v2/"}
	cfg.Normalize()
	if cfg.APIVersion != "/api/v2" {
		t.Fatalf("APIVersion = %q", cfg.APIVersion)
	}
	if cfg.CellBaseURL != "https://cell.example" {
		t.Fatalf("CellBaseURL = %q", cfg.CellBaseURL)
	}
	if cfg.RequestTimeout != bridge.DefaultRequestTimeout || cfg.PollInterval != bridge.DefaultPollInterval || cfg.PollTimeout != bridge.DefaultPollTimeout {
		t.Fatalf("timeouts not defaulted: %+v", cfg)
	}
}

func TestConfigStringOmitsBearer(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{Bearer: testToken, ProjectHost: "gh", Project: "p"}
	for _, got := range []string{cfg.String(), cfg.GoString()} {
		assertNoTokenLeak(t, "Config.String", got)
		if !strings.Contains(got, "token=set") {
			t.Fatalf("Config.String should report the credential presence, got %q", got)
		}
	}
	var nilCfg *bridge.Config
	if got := nilCfg.String(); !strings.Contains(got, "nil") {
		t.Fatalf("nil Config.String = %q", got)
	}
}

func TestNewRejectsInsecureOriginUnlessAllowed(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{CoreBaseURL: "http://core.example", CellBaseURL: "https://cell.example", Bearer: testToken}
	_, err := bridge.New(cfg, nil)
	if !errors.Is(err, bridge.ErrInsecureEndpoint) {
		t.Fatalf("want ErrInsecureEndpoint, got %v", err)
	}

	cfg.AllowHTTP = true
	if _, err := bridge.New(cfg, nil); err != nil {
		t.Fatalf("AllowHTTP should accept an http origin: %v", err)
	}
}

func TestNewRejectsBaseURLWithoutHost(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{CellBaseURL: "https:///api", Bearer: testToken, AllowHTTP: true}
	if _, err := bridge.New(cfg, nil); !errors.Is(err, bridge.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestConfigBearerKeyIsNotMarshaled(t *testing.T) {
	// The private copy is never serialized, but Config has no marshal hook, so
	// confirm the zero value stays empty after Client.Config().
	t.Parallel()
	client, err := bridge.New(&bridge.Config{CoreBaseURL: "https://core.example", CellBaseURL: "https://cell.example", Bearer: testToken}, nil)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	if got := client.Config(); got.Bearer != "" {
		t.Fatalf("Config().Bearer = %q", got.Bearer)
	}
}

func TestProjectPrefixURLEncoding(t *testing.T) {
	t.Parallel()
	cfg := &bridge.Config{ProjectHost: "gh", Project: "a b"}
	cfg.Normalize()
	prefix, err := cfg.ProjectPrefix()
	if err != nil {
		t.Fatalf("ProjectPrefix: %v", err)
	}
	if prefix != "/gh/a%20b" {
		t.Fatalf("ProjectPrefix = %q", prefix)
	}

	empty := &bridge.Config{}
	if _, err := empty.ProjectPrefix(); !errors.Is(err, bridge.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// TestBaseURLIsNeverDerivedFromTheResponsePath guards the allowlist: a stub
// server that answers with a 30x must not move the client elsewhere.
func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	core := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
		http.Redirect(w, r, "http://127.0.0.1:1/steal", http.StatusTemporaryRedirect)
	})
	client := newClient(t, testConfig(core.URL, core.URL), core)
	_, err := client.GetBilling(context.Background(), false)
	if err == nil {
		t.Fatal("expected the redirect to surface as an error")
	}
	var httpErr *bridge.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("want HTTPError 307, got %v", err)
	}
}
