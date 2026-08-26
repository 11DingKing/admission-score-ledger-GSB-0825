// Command server runs the admission score ledger HTTP API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"admission-score-ledger/internal/config"
	"admission-score-ledger/internal/httpserver"
	"admission-score-ledger/internal/repository"
	"admission-score-ledger/internal/service"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatalf("ping database: %v (set DATABASE_URL to a reachable PostgreSQL 16 instance)", err)
	}

	repo := repository.NewPostgresRepository(pool)
	if err := repo.Migrate(ctx); err != nil {
		log.Fatalf("run migrations: %v", err)
	}
	log.Printf("migrations applied")

	ledger := service.NewLedger(repo)
	handler := httpserver.New(ledger)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("admission score ledger listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
	log.Print("server stopped")
	_ = os.Stdout.Sync()
}
