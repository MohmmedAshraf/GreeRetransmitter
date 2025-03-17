package main

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/joomcode/errorx"
	"github.com/rs/zerolog"
	"github.com/sovamorco/gommon/log"
	"github.com/sovamorco/gree-retransmitter/config"
	"github.com/sovamorco/gree-retransmitter/service"
)

const (
	initTimeout = 10 * time.Second
	errBuffer   = 10
)

func run(ctx context.Context, cfg *config.Config) error {
	logger := zerolog.Ctx(ctx)

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cfg.Timeout != 0 {
		var cancelTimeout func()

		ctx, cancelTimeout = context.WithTimeout(ctx, cfg.Timeout)
		defer cancelTimeout()
	}

	errChan := make(chan error, errBuffer)

	srv, err := service.New(logger, cfg, errChan)
	if err != nil {
		return errorx.Decorate(err, "initialize service")
	}

	go srv.Run(logger)

	defer srv.Shutdown(logger)

	logger.Info().Msg("Service started")

	select {
	case err := <-errChan:
		if err != nil {
			return errorx.Decorate(err, "run bots")
		}

		return nil
	case <-ctx.Done():
		logger.Info().Msg("Exiting: Context cancelled")

		return nil
	}
}

func main() {
	initCtx, initCancel := context.WithTimeout(context.Background(), initTimeout)
	defer initCancel()

	logger := log.InitLogger()

	cfg, err := config.LoadConfig(initCtx)
	if err != nil {
		logger.Fatal().Err(err).Msg("Error loading config")
	}

	if cfg.UseDevLogger {
		logger = log.InitDevLogger()
	}

	ctx := logger.WithContext(context.Background())

	err = run(ctx, cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("Error running")
	}
}
