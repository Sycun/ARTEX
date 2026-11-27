package server

import (
	"fmt"
	"log"
	"strings"
)

// DevNoIsolationFlag is the command-line flag of the development escape.
const DevNoIsolationFlag = "dev-insecure-no-isolation"

// The isolation modes the server reports.
const (
	IsolationIsolated = "isolated"
	IsolationDegraded = "degraded"
)

// IsolationStatus is the agent isolation the server runs with.
type IsolationStatus struct {
	Mode string `json:"mode"`
	// Reason is the self-test failure of a degraded start.
	Reason string `json:"reason,omitempty"`
}

// WithIsolationStatus records the result of EnforceIsolation for GET /api/version.
func WithIsolationStatus(st IsolationStatus) Option {
	return func(s *Server) { s.isolation = st }
}

// IsolationTargets are the server's paths and database the self-test checks the
// executor cannot reach.
type IsolationTargets struct {
	DataDir    string
	KeyDir     string
	ConfigPath string
	SkillDir   string
	// DSN is the server's database DSN; only its hosts, ports, user and database
	// name reach the probe, never its password.
	DSN string
}

// EnforceIsolation decides the start from the self-test result, the platform goos
// and the privilege drop p. It returns the status to report, or an error when the
// start must stop.
func EnforceIsolation(goos string, p ExecPrivilege, selfTest error, devEscape bool) (IsolationStatus, error) {
	if selfTest == nil {
		log.Printf("[isolation] self-test passed: agent processes and file tools run as the executor and cannot reach the server's secrets")
		if devEscape {
			log.Printf("[isolation] -%s is set and not needed: the self-test passed", DevNoIsolationFlag)
		}
		return IsolationStatus{Mode: IsolationIsolated}, nil
	}
	if isolationRequired(goos, p) && !devEscape {
		return IsolationStatus{}, fmt.Errorf("agent isolation self-test failed: %v. A Linux server running as root with the privilege drop does not start without agent isolation: fix the cause above, or pass -%s for development only", selfTest, DevNoIsolationFlag)
	}
	logDegradedBanner(selfTest, isolationRequired(goos, p))
	return IsolationStatus{Mode: IsolationDegraded, Reason: selfTest.Error()}, nil
}

func isolationRequired(goos string, p ExecPrivilege) bool {
	return goos == "linux" && p.Enabled
}

func degradedBanner(cause error, escaped bool) []string {
	bar := strings.Repeat("!", 78)
	why := "!! This host cannot isolate agents: that needs Linux and a root server with the" +
		"\n!! privilege drop (-exec-uid), as in the Docker image."
	if escaped {
		why = "!! -" + DevNoIsolationFlag + " is set. DEVELOPMENT ONLY."
	}
	lines := []string{bar, "!! AGENT ISOLATION IS DEGRADED."}
	lines = append(lines, strings.Split(why, "\n")...)
	return append(lines,
		"!! Self-test failure: "+cause.Error(),
		"!! Agent shells, custom tools, MCP stdio servers and the file tools may read the",
		"!! server's keys (JWT signing key, secret key), its config, its database",
		"!! credentials and its process, and may change skills and tools on disk.",
		"!! Do not use this mode on a server that holds real targets or credentials.",
		bar,
	)
}

func logDegradedBanner(cause error, escaped bool) {
	for _, line := range degradedBanner(cause, escaped) {
		log.Print("[isolation] " + line)
	}
}
