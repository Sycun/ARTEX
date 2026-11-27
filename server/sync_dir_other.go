//go:build !unix

package server

func syncDir(path string) error { return nil }
