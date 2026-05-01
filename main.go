package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	rawArgs := os.Args[1:]

	commandLogger, err := newCommandFileLogger()
	if err != nil {
		log.Printf("failed to initialize command logger: %v", err)
	}
	if commandLogger != nil {
		defer commandLogger.Close()
	}

	app := newAppContext()
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("failed to close app context: %v", err)
		}
	}()

	maybeNotifyUpgradeAvailable(ctx, rawArgs, os.Stdout)

	if err := executeCLICommand(ctx, app, commandLogger, rawArgs); err != nil {
		log.Fatal(err)
	}
}

func newRunID() string {
	return fmt.Sprintf("run-%d-%d", time.Now().UnixNano(), os.Getpid())
}
