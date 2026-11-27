// Command artex runs the ARTEX backend: the dual SQLite graph stores,
// the event-driven exploration engine, and the JSON HTTP API consumed by the
// shadcn/ui frontend.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/internal/toolguard"
	"github.com/Autumn-27/artex/selfupdate"
	"github.com/Autumn-27/artex/server"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// printBanner writes the startup banner + version/runtime info to stdout.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI 自主渗透测试系统")
	fmt.Printf("  版本 %s  ·  %s/%s  ·  %s  ·  监听 %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

// main only maps run's result onto the process exit code. The exit code is part
// of the update protocol — the supervising start script reads it to decide
// whether to relaunch us (see selfupdate.ExitRestart) — so the body has to live
// in a function that can *return* rather than os.Exit past its own defers.
func main() {
	if code, handled := toolguard.BackgroundHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	// Before any listener or child exists. On Linux this re-execs once so the
	// kernel environment block drops the startup secrets; the values are restored
	// into this process and scrubbed after New has copied them into memory.
	// macOS has no /proc environ, so SealProcessEnv logs and returns.
	server.SealProcessEnv()
	os.Exit(run())
}

type runtimeConfig struct {
	Addr              string
	DataDir           string
	Proxy             string
	Mode              string
	WorkerID          string
	WorkerZone        string
	WorkerConcurrency int
}

func parseRuntimeConfig(fs *flag.FlagSet, args []string, getenv func(string) string) (runtimeConfig, error) {
	addr := fs.String("addr", ":8787", "HTTP listen address")
	dataDir := fs.String("data", filepath.Join(config.BaseDir(), "data"), "data directory (default: data/ next to the executable)")
	proxy := fs.String("proxy", "127.0.0.1:8788", "traffic recording proxy address (empty to disable)")
	mode := fs.String("mode", "all", "process role: all, api, or worker")
	workerID := fs.String("worker-id", "", "worker identity (default: hostname plus a short suffix)")
	workerZone := fs.String("worker-zone", "default", "execution zone this worker claims")
	workerConcurrency := fs.Int("worker-concurrency", 4, "maximum jobs claimed at once")
	if err := fs.Parse(args); err != nil {
		return runtimeConfig{}, err
	}
	cfg := runtimeConfig{
		Addr:              *addr,
		DataDir:           *dataDir,
		Proxy:             *proxy,
		Mode:              *mode,
		WorkerID:          *workerID,
		WorkerZone:        *workerZone,
		WorkerConcurrency: *workerConcurrency,
	}
	if v := getenv("ARTEX_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := getenv("ARTEX_WORKER_ID"); v != "" {
		cfg.WorkerID = v
	}
	if v := getenv("ARTEX_WORKER_ZONE"); v != "" {
		cfg.WorkerZone = v
	}
	if v := getenv("ARTEX_WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("ARTEX_WORKER_CONCURRENCY: %w", err)
		}
		cfg.WorkerConcurrency = n
	}
	switch cfg.Mode {
	case "all", "api", "worker":
	default:
		return runtimeConfig{}, fmt.Errorf("invalid --mode %q (want all, api, or worker)", cfg.Mode)
	}
	if cfg.WorkerConcurrency < 1 {
		return runtimeConfig{}, fmt.Errorf("worker concurrency must be >= 1")
	}
	if cfg.WorkerZone == "" {
		cfg.WorkerZone = "default"
	}
	if cfg.WorkerID == "" {
		id, err := newWorkerID()
		if err != nil {
			return runtimeConfig{}, err
		}
		cfg.WorkerID = id
	}
	return cfg, nil
}

func newWorkerID() (string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	var buf [3]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return "", err
	}
	return host + "-" + hex.EncodeToString(buf[:]), nil
}

func run() int {
	cfg, err := parseRuntimeConfig(flag.CommandLine, os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}

	// hand the build version to the server package so GET /api/health can report it
	// to the frontend top bar.
	server.BuildVersion = version

	if cfg.Mode == "worker" {
		printBanner(cfg.WorkerID + "@" + cfg.WorkerZone)
	} else {
		printBanner(cfg.Addr)
	}

	// capture backend logs into the in-memory sink (still to stderr) so the /logs
	// page can show a live log stream. Do this first, to catch startup logs too.
	server.StartLogCapture()

	// Self-update bootstrap: swap in a staged binary, or count a post-swap boot
	// attempt and roll back if the new build keeps dying. Must run before we open
	// the stores or bind a port — this may end with "exit and let the start script
	// relaunch me", and there is no point paying for either first.
	action, upState := selfupdate.Bootstrap()
	server.SetBootUpdateState(upState)
	if action == selfupdate.Restart {
		return selfupdate.ExitRestart
	}

	// surface which config file the binary reads (absolute, so `go run`'s relative
	// "config.json" — resolved against the CWD — is unambiguous).
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] 配置文件: %s", cfgPath)
	} else {
		log.Printf("[config] 配置文件: %s (不存在 — 将仅尝试环境变量 ARTEX_PG_DSN)", cfgPath)
	}

	// 页面「恢复备份」暂存的数据必须在任何组件打开数据目录与数据库之前装好。
	skillDir := config.SkillDir()
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	restoreResult, err := server.ApplyPendingRestore(cfg.DataDir, skillDir)
	if err != nil {
		log.Printf("[backup] 恢复文件替换未完成，终止本次启动等待重试：%v", err)
		return 1
	}
	server.SetBootRestoreResult(restoreResult)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var shutdown context.CancelCauseFunc
	srvCtx := sigCtx
	if cfg.Mode == "worker" {
		srvCtx = context.Background()
	} else {
		srvCtx, shutdown = shutdownContext(sigCtx)
		defer shutdown(agent.AbortShutdown)
	}

	// Worker identity is already in cfg. NewManager reads ARTEX_PG_DSN,
	// ARTEX_TRAFFIC_STORE and ARTEX_S3_* into the stores before any scrub.
	trusted, err := server.ParseTrustedProxies(os.Getenv("ARTEX_TRUSTED_PROXIES"))
	if err != nil {
		log.Printf("[config] ARTEX_TRUSTED_PROXIES: %v", err)
		return 1
	}
	mgr, err := server.NewManagerWithMode(cfg.DataDir, cfg.Proxy, cfg.Mode)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()

	// Surviving this long means a freshly swapped-in build actually works, so drop
	// the upgrade marker and stop counting attempts. Until it fires, every boot
	// increments the count and a build that keeps dying gets rolled back.
	settle := time.AfterFunc(selfupdate.SettleDelay, selfupdate.Settle)
	defer settle.Stop()

	log.Printf("[config] skill 目录: %s", skillDir)
	log.Printf("[config] 工具资源限制: %s", toolguard.FromEnv().Summary())
	// NewRuntime reads ARTEX_EVENT_BUS, the LLM credentials and the enterprise
	// OIDC/S3 settings, then scrubs secretEnv. ARTEX_WORKER_* stay in cfg and in
	// the environment, so --mode=worker keeps its identity and does not hand
	// the database or OIDC secrets to a child process.
	srv := server.NewRuntime(srvCtx, mgr, skillDir, cfg.DataDir, config.KeyDir(), server.Runtime{
		Mode:        cfg.Mode,
		WorkerID:    cfg.WorkerID,
		Zone:        cfg.WorkerZone,
		Concurrency: cfg.WorkerConcurrency,
	}, server.WithSecretScrub(), server.WithTrustedProxies(trusted))
	defer srv.Close()
	if cfg.Mode == "worker" {
		log.Printf("ARTEX %s worker %s zone=%s concurrency=%d (data=%s)", version, cfg.WorkerID, cfg.WorkerZone, cfg.WorkerConcurrency, cfg.DataDir)
		if err := srv.RunWorker(sigCtx); err != nil {
			log.Printf("[worker] %v", err)
			return 1
		}
		log.Println("worker stopped")
		return 0
	}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Mode == "all" {
		go func() {
			if err := srv.RunLocalBatches(srvCtx); err != nil && srvCtx.Err() == nil {
				log.Printf("[batch] %v", err)
			}
		}()
	}
	go func() {
		log.Printf("ARTEX %s backend listening on %s (data=%s, workers=%d, mode=%s)", version, cfg.Addr, cfg.DataDir, mgr.Workers(), cfg.Mode)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Two ways out: a signal (normal stop → exit 0, the start script stops looping)
	// or a staged update / rollback (→ exit 75, the script relaunches us and the
	// bootstrap above installs the new build).
	code := 0
	select {
	case <-srvCtx.Done():
	case <-server.RestartRequested():
		code = selfupdate.ExitRestart
		shutdown(agent.AbortShutdown)
	}

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return code
}

// shutdownContext deliberately does not derive from signalCtx. If it did, the
// parent's plain context.Canceled could win the race before AbortShutdown was
// attached to the child, losing the diagnostic cause in every running Agent.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
