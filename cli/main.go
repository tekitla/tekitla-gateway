// Entry point de tequio-gateway: solo wiring + ciclo de vida.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vrojas1015/tequio-gateway/app/auth"
	"github.com/vrojas1015/tequio-gateway/app/config"
	"github.com/vrojas1015/tequio-gateway/app/gateway"
	"github.com/vrojas1015/tequio-gateway/app/middleware"
	"github.com/vrojas1015/tequio-gateway/app/ratelimit"
	"github.com/vrojas1015/tequio-gateway/app/routes"
	"github.com/vrojas1015/tequio-gateway/app/telemetry"
	"github.com/vrojas1015/tequio-gateway/app/upstream"
	"github.com/vrojas1015/tequio-protos/gen/openapi"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case healthcheckCmd: // HEALTHCHECK de docker compose (ver healthcheck.go)
			os.Exit(runHealthcheck(8080))
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuración: %w", err)
	}
	log := telemetry.NewLogger(os.Stdout, cfg.LogFormat, cfg.LogLevel, "tequio-gateway", cfg.ServiceEnv)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1. Tabla de rutas (denegar por defecto).
	table, err := routes.Load(cfg.RoutesFile)
	if err != nil {
		return err
	}
	for _, m := range table.Methods() {
		if p := table.Policy(m); p.Timeout >= cfg.WriteTimeout {
			return fmt.Errorf("routes.yaml: timeout de %s (%s) tiene que ser menor que HTTP_WRITE_TIMEOUT (%s)", m, p.Timeout, cfg.WriteTimeout)
		}
	}

	// 2. Autenticación (respuesta `auth` de la plantilla: firebase).
	authn, challenge, err := newAuthenticator(cfg)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// 3. Rate limit en memoria (fase 1; ver app/ratelimit).
	limiter := ratelimit.NewMemory(0)
	go limiter.Run(ctx, time.Minute)

	// 4. Upstreams: una conexión por Name (lazy: no falla si el servicio
	//    todavía no está; el primer request recibe 503).
	upCfg := upstream.Config{TLS: cfg.UpstreamTLS, Auth: cfg.UpstreamAuth}
	conns := map[string]gateway.Upstream{}
	var ups []gateway.Upstream
	for _, svc := range upstream.Services {
		u, ok := conns[svc.Name]
		if !ok {
			conn, err := upstream.Dial(ctx, upCfg, svc.Name, os.Getenv, gateway.DialOptions()...)
			if err != nil {
				return err
			}
			defer conn.Close()
			u = gateway.Upstream{Conn: conn}
			conns[svc.Name] = u
		}
		ups = append(ups, gateway.Upstream{Service: svc, Conn: u.Conn})
	}

	// 5. Handler.
	var spec []byte
	if cfg.OpenAPIEnabled {
		spec = openapi.Spec
	}
	handler, report, err := gateway.New(ctx, gateway.Options{
		Logger:           log,
		Routes:           table,
		Auth:             authn,
		Limiter:          limiter,
		Upstreams:        ups,
		CORS:             corsConfig(cfg),
		TrustedProxyHops: cfg.TrustedProxyHops,
		OpenAPI:          spec,
		Challenge:        challenge,
	})
	if err != nil {
		return err
	}
	for _, m := range report.Undeclared {
		log.Warn("método con google.api.http SIN entrada en routes.yaml: no se expone (404)", slog.String("method", m))
	}

	// 6. Servidor HTTP con todos los timeouts (slowloris, clientes colgados).
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("gateway escuchando",
			slog.String("addr", srv.Addr),
			slog.String("auth", "firebase"),
			slog.Int("rutas_declaradas", len(table.Methods())),
			slog.Int("rutas_http", len(report.Bindings)),
			slog.Bool("openapi", cfg.OpenAPIEnabled),
		)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("señal de apagado: drenando requests")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Warn("shutdown con requests pendientes", slog.String("error", err.Error()))
		}
	}
	return nil
}

func newAuthenticator(cfg *config.Config) (auth.Authenticator, string, error) {
	v, err := auth.NewFirebase(cfg.FirebaseProjectID, auth.FirebaseOptions{
		JWTOptions: auth.JWTOptions{RolesClaim: cfg.RolesClaim, Leeway: cfg.ClockSkew},
	})
	return v, "Bearer", err
}

func corsConfig(cfg *config.Config) middleware.CORSConfig {
	return middleware.CORSConfig{
		AllowedOrigins:   cfg.CORSAllowedOrigins,
		AllowCredentials: cfg.CORSAllowCredentials,
	}
}
