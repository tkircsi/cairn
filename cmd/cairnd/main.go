// Command cairnd serves the blob endpoints of the OCI distribution API over a
// filesystem blob store and a SQLite index.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/logging"
	"github.com/tkircsi/cairn/internal/metastore/sqlite"
	"github.com/tkircsi/cairn/internal/ocihttp"
	"github.com/tkircsi/cairn/internal/registry"
	"github.com/tkircsi/cairn/internal/uploadstore"
)

func main() {
	if err := run(); err != nil {
		// The default logger, deliberately: run may have failed while building the
		// configured one, and this must still reach stderr.
		slog.Error("cairnd failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr    = flag.String("addr", "127.0.0.1:5050", "address to listen on")
		root    = flag.String("root", "data", "directory for blobs, uploads and the index")
		maxBlob = flag.Int64("max-blob-size", registry.DefaultMaxBlobSize, "largest blob accepted, in bytes")
		maxMani = flag.Int64("max-manifest-size", registry.DefaultMaxManifestSize,
			"largest manifest accepted, in bytes")
		logFormat = flag.String("log-format", "text", "log format: text or json")
		logLevel  = flag.String("log-level", "info", "log level: debug, info, warn or error")
	)

	flag.Parse()

	logger, err := logging.New(os.Stderr, *logFormat, *logLevel)
	if err != nil {
		return err
	}

	// Set as the default too, so anything reaching for slog.Default lands in the
	// same stream with the same format instead of writing unstructured lines.
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	blobs, err := blobstore.NewFS(filepath.Join(*root, "blobs"))
	if err != nil {
		return err
	}

	uploads, err := uploadstore.NewFS(filepath.Join(*root, "uploads"))
	if err != nil {
		return err
	}

	meta, err := sqlite.Open(ctx, filepath.Join(*root, "cairn.db"))
	if err != nil {
		return err
	}

	defer meta.Close()

	reg := registry.New(blobs, uploads, meta,
		registry.WithMaxBlobSize(*maxBlob),
		registry.WithMaxManifestSize(*maxMani),
		registry.WithLogger(logger),
	)

	server := &http.Server{
		Addr:    *addr,
		Handler: ocihttp.WithLogging(ocihttp.NewHandler(reg), logger),
		// A blob upload is a long request by nature, so there is no write or read
		// deadline to cut it short. The header deadline still applies, which is
		// what a slow-loris needs to be held open.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errs := make(chan error, 1)

	go func() {
		logger.Info("listening", "addr", *addr, "root", *root)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("serve: %w", err)

			return
		}

		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx)
}
