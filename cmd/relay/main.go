package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"example.invalid/tunnel-hub-server/internal/admin"
	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/config"
	desktopapi "example.invalid/tunnel-hub-server/internal/desktop"
	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.LoadRelayConfigStrict()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate db: %v", err)
	}
	if cfg.AdminPassword != "" {
		user, created, err := db.EnsureAdminUser(context.Background(), cfg.AdminUsername, cfg.AdminPassword)
		if err != nil {
			log.Fatalf("bootstrap admin user: %v", err)
		}
		if created {
			logger.Info("created bootstrap admin user", "username", user.Username)
		}
	} else if count, err := db.AdminUserCount(context.Background()); err == nil && count == 0 {
		logger.Info("no local admin users configured; set ADMIN_USERNAME and ADMIN_PASSWORD to enable direct admin login")
	}
	manager := proxy.NewManager()
	ssoJWT, err := auth.NewSSOJWTVerifier(auth.SSOJWTConfig{
		Issuer:           cfg.SSOJWTIssuer,
		Audience:         cfg.SSOJWTAudience,
		UserIDClaim:      cfg.SSOJWTUserIDClaim,
		AllowAnyAudience: cfg.SSOJWTAllowAnyAudience,
		PublicKeyFile:    cfg.SSOJWTPublicKeyFile,
		PublicKeyPEM:     cfg.SSOJWTPublicKeyPEM,
	})
	if err != nil {
		log.Fatalf("configure SSO JWT verifier: %v", err)
	}
	relay := proxy.NewRelay(db, manager, logger, cfg.BrandID, cfg.DesktopPublicBaseDomain, cfg.WebAppPublicBaseDomain, cfg.MaxRequestBodyBytes)
	relay.SetDesktopIdentityVerifier(ssoJWT, cfg.SSOJWTAllowMissingScope)
	relay.SetMobileWebAppCookieSecure(cfg.MobileWebAppCookieSecure)
	relay.SetTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	adminServer, err := admin.NewServer(db, manager, cfg, logger, ssoJWT)
	if err != nil {
		log.Fatalf("configure admin server: %v", err)
	}
	desktopServer, err := desktopapi.NewServer(db, cfg, logger, ssoJWT)
	if err != nil {
		log.Fatalf("configure desktop server: %v", err)
	}
	conversationAssetHandler := shareassets.NewHandler()
	static := staticHandler(cfg.WebsiteDist)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case r.URL.Path == "/api/upload":
			relay.HandleUpload(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/pull/"):
			relay.HandlePull(w, r)
		case r.URL.Path == "/api/resource":
			relay.HandleResource(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/push/"):
			relay.HandlePush(w, r)
		case r.URL.Path == "/api/download" || strings.HasPrefix(r.URL.Path, "/api/download/"):
			http.NotFound(w, r)
		case r.URL.Path == "/api/components":
			adminServer.ServeComponents(w, r)
		case strings.HasPrefix(r.URL.Path, shareassets.PublicPathPrefix):
			conversationAssetHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/desktop") || strings.HasPrefix(r.URL.Path, "/share/"):
			desktopServer.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/admin"):
			adminServer.ServeHTTP(w, r)
		case cfg.AdminHost != "" && tunnel.NormalizeHost(r.Host) == tunnel.NormalizeHost(cfg.AdminHost) && static != nil:
			static.ServeHTTP(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	})

	logger.Info("relay listening", "addr", cfg.Addr, "db", cfg.DatabasePath)
	if err := http.ListenAndServe(cfg.Addr, root); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

func staticHandler(dist string) http.Handler {
	if dist == "" {
		return nil
	}
	index := filepath.Join(dist, "index.html")
	if _, err := os.Stat(index); err != nil {
		return nil
	}
	fileServer := http.FileServer(http.Dir(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dist, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		if _, err := os.Stat(path); err != nil && errors.Is(err, fs.ErrNotExist) {
			http.ServeFile(w, r, index)
			return
		}
		http.ServeFile(w, r, index)
	})
}
