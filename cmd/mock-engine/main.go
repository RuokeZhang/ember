package main

import (
	"errors"
	"log"
	"net/http"
)

func main() {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	engine := newMockEngine(cfg)
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: engine.Handler(),
	}

	log.Printf("mock engine listening on :%s", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mock engine exited: %v", err)
	}
}
