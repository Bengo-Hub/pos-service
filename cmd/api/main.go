package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	_ "github.com/bengobox/pos-service/internal/http/docs"

	"github.com/bengobox/pos-service/internal/app"
)

// @title POS Service API
// @version 0.1.0
// @description HTTP API for the BengoBox POS service. Provides point-of-sale operations, order management, and cash drawer management.
// @BasePath /api/v1
// @schemes http https
// @securityDefinitions.apikey bearerAuth
// @in header
// @name Authorization
// @description JWT token from auth-service. Format: Bearer {token}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx)
	if err != nil {
		log.Fatalf("failed to initialise app: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("runtime error: %v", err)
	}
}

