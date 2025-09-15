package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type app struct {
	log          *slog.Logger
	pool         *pgxpool.Pool
	upstreamAuth string
	proxy        *httputil.ReverseProxy
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	upToken := os.Getenv("UPSTREAM_TOKEN")
	if strings.TrimSpace(upToken) == "" {
		logger.Error("UPSTREAM_TOKEN is required")
		os.Exit(1)
	}
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		logger.Error("failed to create pgx pool", "error", err)
		os.Exit(1)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		pingCancel()
		logger.Error("database ping failed", "error", err)
		os.Exit(1)
	}
	pingCancel()

	upAuth := normalizeUpstreamAuth(upToken)
	proxy := newProxy(upAuth)

	a := &app{
		log:          logger,
		pool:         pool,
		upstreamAuth: upAuth,
		proxy:        proxy,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.healthHandler)
	mux.Handle("/", http.HandlerFunc(a.handleProxy))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       0,
	}

	go func() {
		a.log.Info("listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		a.log.Error("shutdown error", "error", err)
	}
	pool.Close()
}

func (a *app) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (a *app) handleProxy(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	incomingToken, err := extractIncomingToken(authHeader)
	if err != nil {
		http.Error(w, "missing or malformed Authorization", http.StatusUnauthorized)
		return
	}
	ok, err := a.tokenExists(r.Context(), incomingToken)
	if err != nil {
		a.log.Error("db error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	go a.incrementUsage(incomingToken)
	a.proxy.ServeHTTP(w, r)
}

func (a *app) incrementUsage(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = a.pool.Exec(ctx, "update tokens set request_count = request_count + 1 where token=$1", token)
}

func (a *app) tokenExists(ctx context.Context, token string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var exists bool
	err := a.pool.QueryRow(cctx, "select exists(select 1 from tokens where token=$1 and is_active=true)", token).Scan(&exists)
	return exists, err
}

func newProxy(upstreamAuth string) *httputil.ReverseProxy {
	targetScheme := "https"
	targetHost := os.Getenv("TARGET_HOST")

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetScheme
			pr.Out.URL.Host = targetHost
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawPath = pr.In.URL.RawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = targetHost
			pr.Out.Header.Set("Authorization", upstreamAuth)
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
	return p
}

func extractIncomingToken(h string) (string, error) {
	if strings.TrimSpace(h) == "" {
		return "", errors.New("empty")
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != "" {
		return strings.TrimSpace(parts[1]), nil
	}
	return strings.TrimSpace(h), nil
}

func normalizeUpstreamAuth(tok string) string {
	t := strings.TrimSpace(tok)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t
	}
	return "Bearer " + t
}
