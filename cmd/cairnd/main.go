// Command cairnd runs both surfaces over one store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"

	ociv1 "github.com/tkircsi/cairn/gen/oci/v1"
	"github.com/tkircsi/cairn/internal/blobstore"
	"github.com/tkircsi/cairn/internal/grpcapi"
	"github.com/tkircsi/cairn/internal/metastore/sqlite"
	"github.com/tkircsi/cairn/internal/ocihttp"
	"github.com/tkircsi/cairn/internal/registry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exit", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir  = flag.String("data", "./data", "directory holding blobs and the database")
		httpAddr = flag.String("http", "127.0.0.1:5000", "address for the read-only OCI surface")
		grpcAddr = flag.String("grpc", "127.0.0.1:5001", "address for the write surface")
	)

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	blobs, err := blobstore.NewFS(filepath.Join(*dataDir, "blobs"))
	if err != nil {
		return err
	}

	meta, err := sqlite.Open(ctx, filepath.Join(*dataDir, "cairn.db"))
	if err != nil {
		return err
	}
	defer meta.Close()

	reg := registry.New(blobs, meta)

	grpcServer := grpc.NewServer()
	ociv1.RegisterStoreServiceServer(grpcServer, grpcapi.NewServer(reg))

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           ocihttp.NewHandler(reg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 2)

	go func() {
		listener, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			errs <- fmt.Errorf("listen grpc: %w", err)

			return
		}

		slog.Info("write surface listening", "addr", *grpcAddr, "protocol", "grpc")

		errs <- grpcServer.Serve(listener)
	}()

	go func() {
		slog.Info("read surface listening", "addr", *httpAddr, "protocol", "oci-http")

		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		errs <- err
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errs:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	grpcServer.GracefulStop()

	return httpServer.Shutdown(shutdownCtx)
}
