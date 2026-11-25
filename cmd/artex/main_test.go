package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestShutdownContextPreservesNamedCause(t *testing.T) {
	signalCtx, signalCancel := context.WithCancel(context.Background())
	ctx, shutdown := shutdownContext(signalCtx)
	defer shutdown(nil)
	signalCancel()
	<-ctx.Done()
	if code, _, _, ok := agent.AbortReason(ctx); !ok || code != "shutdown" {
		t.Fatalf("code=%q ok=%v, want shutdown", code, ok)
	}
}

func TestParseRuntimeModes(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		mode    string
		zone    string
		id      string
		conc    int
		wantErr string
	}{
		{name: "default all", mode: "all", zone: "default", conc: 4},
		{name: "flag api", args: []string{"--mode=api", "--worker-zone=intranet", "--worker-concurrency=2", "--worker-id=api-1"}, mode: "api", zone: "intranet", id: "api-1", conc: 2},
		{name: "flag worker", args: []string{"--mode=worker"}, mode: "worker", zone: "default", conc: 4},
		{name: "env overrides flags", args: []string{"--mode=all", "--worker-zone=default", "--worker-concurrency=4", "--worker-id=flag"}, env: map[string]string{"ARTEX_MODE": "worker", "ARTEX_WORKER_ZONE": "intranet", "ARTEX_WORKER_CONCURRENCY": "9", "ARTEX_WORKER_ID": "env-node"}, mode: "worker", zone: "intranet", id: "env-node", conc: 9},
		{name: "bad mode", args: []string{"--mode=both"}, wantErr: "invalid --mode"},
		{name: "bad env mode", env: map[string]string{"ARTEX_MODE": "scheduler"}, wantErr: "invalid --mode"},
		{name: "bad concurrency", args: []string{"--worker-concurrency=0"}, wantErr: "concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("artex", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			cfg, err := parseRuntimeConfig(fs, tc.args, func(key string) string { return tc.env[key] })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != tc.mode || cfg.WorkerZone != tc.zone || cfg.WorkerConcurrency != tc.conc {
				t.Fatalf("got mode=%s zone=%s conc=%d", cfg.Mode, cfg.WorkerZone, cfg.WorkerConcurrency)
			}
			if tc.id != "" && cfg.WorkerID != tc.id {
				t.Fatalf("worker id=%s want %s", cfg.WorkerID, tc.id)
			}
			if tc.id == "" && !strings.HasPrefix(cfg.WorkerID, host+"-") {
				t.Fatalf("generated worker id %q does not start with %s-", cfg.WorkerID, host)
			}
		})
	}
}
