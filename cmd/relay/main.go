package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.LoadRelayConfigStrict()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	db, err := store.Open(context.Background(), cfg.MySQL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	startup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(startup); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	if cfg.AdminPassword != "" {
		user, created, err := db.EnsureAdminUser(startup, cfg.AdminUsername, cfg.AdminPassword)
		if err != nil {
			return fmt.Errorf("bootstrap admin user: %w", err)
		}
		if created {
			logger.Info("created bootstrap admin user", "username", user.Username)
		}
	} else {
		count, err := db.AdminUserCount(startup)
		if err != nil {
			return fmt.Errorf("count admin users: %w", err)
		}
		if count == 0 {
			logger.Info("no local admin users configured; set ADMIN_USERNAME and ADMIN_PASSWORD to enable direct admin login")
		}
	}
	cancel()
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
		return fmt.Errorf("configure SSO JWT verifier: %w", err)
	}
	relay := proxy.NewRelay(db, manager, logger, cfg.BrandID, cfg.DesktopPublicBaseDomain, cfg.WebAppPublicBaseDomain, cfg.MaxRequestBodyBytes)
	relay.SetDesktopIdentityVerifier(ssoJWT, cfg.SSOJWTAllowMissingScope)
	relay.SetMobileWebAppCookieSecure(cfg.MobileWebAppCookieSecure)
	relay.SetTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	adminServer, err := admin.NewServer(db, manager, cfg, logger, ssoJWT)
	if err != nil {
		return fmt.Errorf("configure admin server: %w", err)
	}
	desktopServer, err := desktopapi.NewServer(db, cfg, logger, ssoJWT)
	if err != nil {
		return fmt.Errorf("configure desktop server: %w", err)
	}
	conversationAssets := shareassets.NewBundle()
	desktopServer.SetConversationShareRenderer(conversationAssets)
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
			conversationAssets.ServeHTTP(w, r)
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

	logger.Info("relay listening", "addr", cfg.Addr, "mysql_host", cfg.MySQL.Host, "mysql_port", cfg.MySQL.Port, "mysql_database", cfg.MySQL.Database)
	if err := http.ListenAndServe(cfg.Addr, root); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
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
