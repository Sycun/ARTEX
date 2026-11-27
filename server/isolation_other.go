//go:build !unix

package server

func IsolationSelfTest(p ExecPrivilege, t IsolationTargets) error {
	return errExecUnsupported
}
