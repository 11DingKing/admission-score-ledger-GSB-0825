// Command server starts the admission score ledger HTTP server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/gsb/admission-score-ledger/internal/config"
	"github.com/gsb/admission-score-ledger/internal/httpserver"
	"github.com/gsb/admission-score-ledger/internal/repository"
	"github.com/gsb/admission-score-ledger/internal/service"
)

func main() {
	logger := log.New(os.Stdout, "[ledger] ", log.LstdFlags|log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		logger.Fatalf("failed to ping database: %v", err)
	}

	if err := repository.RunMigrations(context.Background(), db); err != nil {
		logger.Fatalf("failed to run migrations: %v", err)
	}
	logger.Println("migrations applied successfully")

	repo := repository.NewRepo(db)
	svc := service.NewService(repo)
	handler := httpserver.NewHandler(svc)

	mux := handler.Routes()
	var h http.Handler = mux
	h = httpserver.JSONContentTypeMiddleware(h)
	h = httpserver.TraceMiddleware(h)
	h = httpserver.RecoveryMiddleware(h)
	h = httpserver.LoggingMiddleware(logger, h)

	srv := &http.Server{
		Addr:         cfg.ServerAddr,
		Handler:      h,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	dispatcher := service.NewOutboxDispatcher(repo, logger)
	dispatcherCtx, dispatcherCancel := context.WithCancel(context.Background())
	go dispatcher.Run(dispatcherCtx, cfg.OutboxInterval)

	go func() {
		logger.Printf("server listening on %s", cfg.ServerAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Println("shutting down...")

	dispatcherCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("server shutdown error: %v", err)
	}
	logger.Println("server stopped")
}
