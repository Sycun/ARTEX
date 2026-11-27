package server

import "os"

// secretEnv lists credentials that are copied into process memory at startup
// and then removed from the environment. Child processes (agent shells, custom
// tools, MCP stdio servers) copy os.Environ() when they are spawned.
//
// Operational settings are intentionally absent: ARTEX_MODE, ARTEX_EVENT_BUS,
// ARTEX_TRAFFIC_STORE, ARTEX_WORKER_ID, ARTEX_WORKER_ZONE,
// ARTEX_WORKER_CONCURRENCY, ARTEX_ENTERPRISE, ARTEX_PUBLIC_ORIGIN and the OIDC
// issuer/client id are configuration, not credentials. Scrubbing them would
// drop --mode=worker identity and the distributed store/bus selection.
//
// Readers that must run before ScrubSecretEnv:
//   - config / db.DSN: ARTEX_PG_DSN (NewManager, before New returns)
//   - traffic.Backend / OpenBlobStore: ARTEX_TRAFFIC_STORE, ARTEX_S3_* (NewManager)
//   - NewRuntime: ARTEX_EVENT_BUS, agent.FromEnv (LLM keys and credential URLs)
//   - bootEnterprise / enterprise.New: ARTEX_OIDC_CLIENT_SECRET, ARTEX_S3_* again
//
// ARTEX_LLM_PROXY and ARTEX_LLM_BASE_URL are URLs that can carry credentials in
// their userinfo. GEMINI_API_KEY and GOOGLE_API_KEY are the native Gemini
// credentials FromEnv reads. AWS_* names are scrubbed so a host credential
// chain is not copied into agent children; this tree does not construct an
// AWS SDK client from them.
var secretEnv = []string{
	"ARTEX_PG_DSN", "ARTEX_PG_PASSWORD", "ARTEX_SECRET_KEY",
	"ARTEX_OIDC_CLIENT_SECRET",
	"ARTEX_S3_ACCESS_KEY", "ARTEX_S3_SECRET_KEY",
	"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
	"ARTEX_LLM_API_KEY", "ARTEX_LLM_CUSTOM_HEADERS",
	"ARTEX_LLM_PROXY", "ARTEX_LLM_BASE_URL",
	"ARTEX_ARCHIVE_S3_ACCESS_KEY_ID", "ARTEX_ARCHIVE_S3_SECRET_ACCESS_KEY", "ARTEX_ARCHIVE_S3_SESSION_TOKEN",
	"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
}

// ScrubSecretEnv removes secretEnv from the process environment once startup
// has read it. Call it only after NewManager and bootEnterprise have copied
// the distributed and enterprise settings into memory, and before any goroutine
// that can spawn a child (MCP discovery, restored workers).
func ScrubSecretEnv() {
	for _, k := range secretEnv {
		_ = os.Unsetenv(k)
	}
}

// Option configures New. Options keep the test call sites unchanged: only the
// binary opts into scrubbing, because tests create several servers in one
// process and still need ARTEX_PG_DSN on later calls.
type Option func(*Server)

// WithSecretScrub makes New drop the startup secrets after reading them,
// before MCP discovery and task restore can spawn children.
func WithSecretScrub() Option { return func(s *Server) { s.scrubSecrets = true } }
