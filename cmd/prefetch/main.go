package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/RuokeZhang/ember/cmd/prefetch/prefetch"
)

func main() {
	var options prefetch.Options
	flag.StringVar(&options.Root, "root", "", "Absolute cache root path.")
	flag.StringVar(&options.CacheHash, "cache-hash", "", "Deterministic cache hash directory name.")
	flag.StringVar(&options.ExpectedDigest, "expected-digest", "", "Expected sha256 digest for the synthetic safetensors artifact.")
	flag.Int64Var(&options.ExpectedSize, "expected-size", 0, "Expected artifact size in bytes.")
	flag.BoolVar(&options.Synthetic, "synthetic", false, "Write the repository-owned synthetic safetensors artifact.")
	flag.BoolVar(&options.VerifyOnly, "verify-only", false, "Only verify an existing cache directory.")
	flag.BoolVar(&options.PrepareRoot, "prepare-root", false, "Prepare the cache root for the non-root prefetch process.")
	flag.Parse()
	options.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := prefetch.Run(context.Background(), options); err != nil {
		options.Logger.Error("prefetch command failed", "error", err)
		os.Exit(1)
	}
}
