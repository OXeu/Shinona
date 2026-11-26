package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/adapter/terminal"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func init() {
	if _, configured := os.LookupEnv(gin.EnvGinMode); !configured {
		gin.SetMode(gin.ReleaseMode)
	}
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := execute(ctx, logger, os.Args[1:])
	stop()
	if err != nil {
		logger.Error("shinona stopped", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, logger *slog.Logger, environment environment) error {
	hasher, err := passwordHasher(environment)
	if err != nil {
		return err
	}
	address, err := listenAddress(environment)
	if err != nil {
		return err
	}
	proxies, err := trustedProxyNetworks(environment.value("SHINONA_TRUSTED_PROXIES", ""))
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()

	repository, err := sqlite.Open(ctx, environment.value("SHINONA_DATABASE", "shinona.db"))
	if err != nil {
		return fmt.Errorf("open SQLite database: %w", err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			logger.Error("close SQLite database", "error", err)
		}
	}()

	if err := repository.FailInterrupted(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("recover interrupted runs: %w", err)
	}

	config, err := newRuntime(ctx, repository, environment)
	if err != nil {
		return err
	}
	controls := &run.Controls{}
	service := run.NewService(repository, config.ResolveForAccount, uuid.NewString, controls, config.ProjectTargets)
	defer service.CloseTerminals()

	authConfig := func() httpauth.Config {
		current := config.Auth()
		current.AccountDeleted = service.AccountDeleted
		current.TrustedProxies = proxies
		return current
	}
	authHandler := httpauth.NewWithConfig(auth.New(repository, hasher, uuid.NewString), authConfig, logger)
	router := newAPIRouter(ctx, config.origin.String(), service, config, authHandler, logger)
	config.mcpOAuth.Routes(httpserver.GinRoutes{IRoutes: router}, authHandler.RequireUser, sameOriginJSON,
		config.lookupMCP, config.origin.Scheme == "https", logger)
	server := &http.Server{
		Handler:           httpserver.AuthenticatedServerTiming(allowedHost(router, config.Host)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      run.ProjectSandboxTimeout + 15*time.Second,
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		run.NewWorker(repository, config.ResolveForAccount, logger, controls, run.DefaultWorkerConcurrency).Serve(workerCtx)
	}()

	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	logger.Info("shinona API ready", "address", listener.Addr().String(), "mode", config.Mode())

	select {
	case err := <-serverDone:
		stopWorker()
		<-workerDone
		return err
	case <-ctx.Done():
		stopWorker()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		<-workerDone
		return errors.Join(shutdownErr, ignoreServerClosed(<-serverDone))
	}
}

func newAPIRouter(ctx context.Context, origin string, service *run.Service, app rest.Application,
	authHandler *httpauth.Handler, logger *slog.Logger,
) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.HandleMethodNotAllowed = true
	router.Use(gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		logger.Error("HTTP handler panicked", "error", recovered, "stack", string(debug.Stack()))
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	routes := httpserver.GinRoutes{IRoutes: router}

	authHandler.Routes(routes, sameOriginJSON)
	terminal.Routes(routes, ctx, origin, service, authHandler.RequireUser, sameOriginJSON, authHandler.SessionUser, logger)
	transfer := sessiontransfer.New(service, logger)
	routes.Handle("GET "+sessiontransfer.LivePath, authHandler.RequireUser(transfer.Live(ctx, origin, authHandler.SessionUser, service)))
	routes.Handle("GET "+sessiontransfer.AttachmentPath, authHandler.RequireUser(sessiontransfer.Attachment(service, logger)))

	api := router.Group(rest.Prefix, httpserver.Compress, authHandler.RequireGinUser)
	rest.Register(api, service, app, logger)
	return router
}

func allowedHost(next http.Handler, configuredHost func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = strings.Trim(r.Host, "[]")
		}
		if !isLoopback(host) && !strings.EqualFold(r.Host, configuredHost()) {
			http.Error(w, "Request host is unavailable.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginJSON(next http.Handler) http.Handler {
	protected := http.NewCrossOriginProtection().Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			http.Error(w, "An application/json body is required.", http.StatusUnsupportedMediaType)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func ignoreServerClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
