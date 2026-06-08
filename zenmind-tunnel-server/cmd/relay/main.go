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

	"github.com/linlay/zenmind-tunnel-server/internal/admin"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.LoadRelayConfig()

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate db: %v", err)
	}
	if err := db.BootstrapAdmin(context.Background(), cfg.BootstrapAdminUsername, cfg.BootstrapAdminPassword); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}

	manager := proxy.NewManager()
	relay := proxy.NewRelay(db, manager, logger, cfg.MaxRequestBodyBytes)
	adminServer := admin.NewServer(db, manager, cfg, logger)
	static := staticHandler(cfg.WebsiteDist)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
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
