// Command deeptutor_acp_bridge serves the Hermes gateway contract for DeepTutor
// and forwards runs to an ACP agent launched as a stdio subprocess.
//
//	DEEPTUTOR_ACP_BRIDGE_KEY=secret deeptutor_acp_bridge -listen 127.0.0.1:8642 -- gemini --experimental-acp
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/j75689/deeptutor_acp_bridge/internal/bridge"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8642", "address to listen on")
	cwd := flag.String("cwd", ".", "working directory for ACP sessions")
	keyEnv := flag.String("key-env", "DEEPTUTOR_ACP_BRIDGE_KEY", "env var holding the bearer token")
	authMethod := flag.String("auth-method", "", "ACP auth method id to authenticate with after initialize (e.g. gemini-api-key)")
	oauthRelay := flag.String("oauth-relay", "", "port range (lo-hi) to relay from the container address to the agent's loopback, for browser sign-in callbacks (e.g. 38000-38031)")
	label := flag.String("model", "", "name reported by /v1/capabilities (default: agent's own name)")
	flag.Usage = func() {
		flag.PrintDefaults()
		os.Stderr.WriteString("\nusage: deeptutor_acp_bridge [flags] -- <acp-agent-command> [args...]\n")
	}
	flag.Parse()

	key := os.Getenv(*keyEnv)
	if key == "" {
		slog.Error("bearer token env var is empty", "env", *keyEnv)
		os.Exit(2)
	}
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if *oauthRelay != "" {
		lo, hi, err := bridge.ParsePortRange(*oauthRelay)
		if err != nil {
			slog.Error("bad -oauth-relay", "err", err)
			os.Exit(2)
		}
		relay := &bridge.Relay{Lo: lo, Hi: hi}
		if err := relay.Start(context.Background()); err != nil {
			slog.Error("oauth relay", "err", err)
			os.Exit(1)
		}
		defer relay.Close()
	}

	b := bridge.New(bridge.Config{
		AgentCmd:   flag.Args(),
		Cwd:        *cwd,
		APIKey:     key,
		AuthMethod: *authMethod,
		ModelLabel: *label,
	})
	defer b.Close()

	srv := &http.Server{Addr: *listen, Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", *listen, "agent", flag.Args())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
