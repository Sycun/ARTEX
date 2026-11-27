package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// A child process spawned after startup must not see the startup secrets, while
// distributed settings still pass through.
func TestScrubSecretEnvHidesSecretsFromChildren(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh in PATH")
	}
	for _, k := range secretEnv {
		t.Setenv(k, "leak-"+k)
	}
	for _, k := range []string{"ARTEX_EVENT_BUS", "ARTEX_TRAFFIC_STORE", "ARTEX_WORKER_ID", "ARTEX_WORKER_ZONE", "ARTEX_WORKER_CONCURRENCY", "ARTEX_MODE"} {
		t.Setenv(k, "kept-"+k)
	}

	ScrubSecretEnv()

	out, err := exec.Command(sh, "-c", "env").Output()
	if err != nil {
		t.Fatal(err)
	}
	env := "\n" + string(out)
	for _, k := range secretEnv {
		if strings.Contains(env, "\n"+k+"=") {
			t.Errorf("%s still reaches child processes", k)
		}
	}
	for _, k := range []string{"ARTEX_EVENT_BUS", "ARTEX_TRAFFIC_STORE", "ARTEX_WORKER_ID", "ARTEX_WORKER_ZONE", "ARTEX_WORKER_CONCURRENCY", "ARTEX_MODE"} {
		if !strings.Contains(env, "\n"+k+"=kept-"+k+"\n") {
			t.Errorf("distributed setting %s did not reach the child process", k)
		}
	}
}

func TestScrubSecretEnvClearsEnterpriseAndLLMCredentials(t *testing.T) {
	for _, k := range secretEnv {
		t.Setenv(k, os.Getenv(k))
	}
	for _, k := range []string{"ARTEX_OIDC_CLIENT_SECRET", "ARTEX_S3_SECRET_KEY", "ARTEX_S3_ACCESS_KEY", "GEMINI_API_KEY", "ARTEX_LLM_API_KEY", "ARTEX_LLM_CUSTOM_HEADERS"} {
		t.Setenv(k, "synthetic-"+k)
	}
	for _, k := range []string{"ARTEX_EVENT_BUS", "ARTEX_TRAFFIC_STORE", "ARTEX_WORKER_ID"} {
		t.Setenv(k, "still-set")
	}

	ScrubSecretEnv()

	for _, k := range []string{"ARTEX_OIDC_CLIENT_SECRET", "ARTEX_S3_SECRET_KEY", "ARTEX_S3_ACCESS_KEY", "GEMINI_API_KEY", "ARTEX_LLM_API_KEY", "ARTEX_LLM_CUSTOM_HEADERS"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set after ScrubSecretEnv", k)
		}
	}
	for _, k := range []string{"ARTEX_EVENT_BUS", "ARTEX_TRAFFIC_STORE", "ARTEX_WORKER_ID"} {
		if os.Getenv(k) != "still-set" {
			t.Errorf("%s = %q, want still-set", k, os.Getenv(k))
		}
	}
}

// New must apply the options it receives. An earlier version accepted
// ...Option and never called them, so the binary kept the secrets in the
// environment while the unit test above still passed.
func TestNewAppliesSecretScrubOption(t *testing.T) {
	// t.Setenv restores both after the test: the scrub unsets them for the rest
	// of this process, and later tests still need the DSN.
	if dsn := os.Getenv("ARTEX_PG_DSN"); dsn != "" {
		t.Setenv("ARTEX_PG_DSN", dsn)
	}
	t.Setenv("ARTEX_SECRET_KEY", os.Getenv("ARTEX_SECRET_KEY"))
	t.Setenv("ANTHROPIC_API_KEY", "scrub-probe")
	t.Setenv("OPENAI_API_KEY", "scrub-probe")
	t.Setenv("ARTEX_OIDC_CLIENT_SECRET", "scrub-probe")
	t.Setenv("ARTEX_EVENT_BUS", "memory")
	t.Setenv("ARTEX_TRAFFIC_STORE", "sqlite")
	t.Setenv("ARTEX_WORKER_ID", "scrub-worker")

	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("no manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir(), WithSecretScrub())

	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("ARTEX_OIDC_CLIENT_SECRET") != "" {
		t.Fatal("New did not apply WithSecretScrub: the secrets are still in the environment")
	}
	if os.Getenv("ARTEX_EVENT_BUS") != "memory" || os.Getenv("ARTEX_TRAFFIC_STORE") != "sqlite" || os.Getenv("ARTEX_WORKER_ID") != "scrub-worker" {
		t.Fatalf("scrub dropped distributed config: bus=%q store=%q worker=%q", os.Getenv("ARTEX_EVENT_BUS"), os.Getenv("ARTEX_TRAFFIC_STORE"), os.Getenv("ARTEX_WORKER_ID"))
	}
}

// The LLM proxy and endpoint URLs can carry user:pass@ credentials. They are
// scrubbed like the keys, and a child spawned after the scrub sees neither,
// while agent.FromEnv still read them before it.
func TestSecretEnvCoversCredentialURLs(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh in PATH")
	}
	for _, k := range []string{"ARTEX_LLM_PROXY", "ARTEX_LLM_BASE_URL", "ARTEX_OIDC_CLIENT_SECRET", "ARTEX_S3_SECRET_KEY"} {
		if !isSecretName(k) {
			t.Errorf("%s is not in secretEnv", k)
		}
	}
	for _, k := range []string{"ARTEX_EVENT_BUS", "ARTEX_TRAFFIC_STORE", "ARTEX_WORKER_ID", "ARTEX_WORKER_ZONE", "ARTEX_WORKER_CONCURRENCY", "ARTEX_ENTERPRISE"} {
		if isSecretName(k) {
			t.Errorf("%s must stay in the environment so worker mode keeps its configuration", k)
		}
	}
	// ScrubSecretEnv unsets every name in secretEnv for the rest of the test
	// binary, and t.Setenv restores only the names it set. Setting all of them
	// here gives ARTEX_PG_DSN and ARTEX_SECRET_KEY back to the tests that follow.
	for _, k := range secretEnv {
		t.Setenv(k, "probe-"+k)
	}
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ARTEX_LLM_PROXY", "http://proxyuser:proxypass@proxy.internal:3128")
	t.Setenv("ARTEX_LLM_BASE_URL", "https://apiuser:apipass@llm.internal/v1")
	cfg, ok := agent.FromEnv()
	if !ok || cfg.Proxy != "http://proxyuser:proxypass@proxy.internal:3128" || cfg.BaseURL != "https://apiuser:apipass@llm.internal/v1" {
		t.Fatalf("FromEnv must read the URLs before the scrub: ok=%v proxy=%q base=%q", ok, cfg.Proxy, cfg.BaseURL)
	}
	ScrubSecretEnv()
	out, err := exec.Command(sh, "-c", "env").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "proxypass") || strings.Contains(string(out), "apipass") {
		t.Fatalf("a child process still sees the proxy or endpoint credentials:\n%s", out)
	}
}
