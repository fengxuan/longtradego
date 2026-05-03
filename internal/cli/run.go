package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"longtradego/internal/core"
	"longtradego/internal/service"
)

func Run(rawArgs []string) error {
	ctx := context.Background()

	commandLogger, err := core.NewCommandFileLogger()
	if err != nil {
		log.Printf("failed to initialize command logger: %v", err)
	}
	if commandLogger != nil {
		defer commandLogger.Close()
	}

	app := core.NewAppContext()
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("failed to close app context: %v", err)
		}
	}()

	service.SetCLIExecutor(executeCLICommand)
	service.MaybeNotifyUpgradeAvailable(ctx, rawArgs, os.Stdout)
	return executeCLICommand(ctx, app, commandLogger, rawArgs)
}

func newRunID() string {
	return fmt.Sprintf("run-%d-%d", time.Now().UnixNano(), os.Getpid())
}
