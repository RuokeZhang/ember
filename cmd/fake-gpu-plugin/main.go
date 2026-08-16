package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	plugin, err := newFakeGPUPlugin(cfg)
	if err != nil {
		log.Fatalf("initialize plugin: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("starting fake GPU plugin with %d devices on %s", cfg.DeviceCount, cfg.PluginSocketDir)
	if err := plugin.Run(ctx); err != nil {
		log.Fatalf("plugin exited: %v", err)
	}
}
