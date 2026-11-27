//go:build !unix

package server

import (
	"errors"
	"os"
	"os/exec"
)

var errExecUnsupported = errors.New("agent process isolation is not supported on this platform")

func execPrivilegeSupported() bool { return false }

func serverIsRoot() bool { return false }

func applyExecCredential(c *exec.Cmd, p ExecPrivilege) {}

func agentProcessEnv(p ExecPrivilege) []string { return os.Environ() }

func withExecKeep(p ExecPrivilege, env map[string]string) map[string]string { return env }

func prepareExecScript(p ExecPrivilege, path string) error { return nil }

func shareRestoredTaskDir(p ExecPrivilege, taskDir string) error { return nil }

func shareRestoredSpillDir(p ExecPrivilege, dir string) error { return nil }

func PrepareExecPrivilege(p *ExecPrivilege, dataDir, keyDir, configPath string) error {
	if p.Enabled {
		return errExecUnsupported
	}
	return nil
}

func InstallBashShim(p ExecPrivilege) error {
	if p.Enabled {
		return errExecUnsupported
	}
	return nil
}

func RunExecShimIfRequested(argv []string) {}
