// Package grpcapi serves the write surface.
//
// The handlers are thin on purpose: every rule about what may be stored lives in
// the registry service, so this package only translates between protobuf and the
// internal model.
package grpcapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/opencontainers/go-digest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ociv1 "github.com/tkircsi/cairn/gen/oci/v1"
	"github.com/tkircsi/cairn/internal/registry"
)

// Server implements the StoreService.
type Server struct {
	ociv1.UnimplementedStoreServiceServer

	reg *registry.Registry
}

// NewServer builds a Server over reg.
func NewServer(reg *registry.Registry) *Server {
	return &Server{reg: reg}
}

func (s *Server) PutBlob(ctx context.Context, req *ociv1.PutBlobRequest) (*ociv1.PutBlobResponse, error) {
	if req.GetRepository() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository is required")
	}

	dgst, err := s.reg.PutBlob(ctx, req.GetRepository(), req.GetData())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "put blob: %v", err)
	}

	return &ociv1.PutBlobResponse{
		Digest: dgst.String(),
		Size:   int64(len(req.GetData())),
	}, nil
}

func (s *Server) PutManifest(ctx context.Context, req *ociv1.PutManifestRequest) (*ociv1.PutManifestResponse, error) {
	if req.GetRepository() == "" {
		return nil, status.Error(codes.InvalidArgument, "repository is required")
	}

	dgst, subject, err := s.reg.PutManifest(ctx, req.GetRepository(), req.GetData())
	if err != nil {
		// A manifest that does not parse is the caller's fault, not ours.
		return nil, status.Errorf(codes.InvalidArgument, "put manifest: %v", err)
	}

	return &ociv1.PutManifestResponse{
		Digest:  dgst.String(),
		Subject: subject.String(),
	}, nil
}

func (s *Server) GetManifest(ctx context.Context, req *ociv1.GetManifestRequest) (*ociv1.GetManifestResponse, error) {
	dgst, err := digest.Parse(req.GetDigest())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid digest")
	}

	data, mediaType, err := s.reg.Manifest(ctx, req.GetRepository(), dgst)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "manifest not found")
		}

		return nil, status.Errorf(codes.Internal, "get manifest: %v", err)
	}

	return &ociv1.GetManifestResponse{Data: data, MediaType: mediaType}, nil
}

func (s *Server) DeleteManifest(ctx context.Context, req *ociv1.DeleteManifestRequest) (*ociv1.DeleteManifestResponse, error) {
	dgst, err := digest.Parse(req.GetDigest())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid digest")
	}

	if err := s.reg.DeleteManifest(ctx, req.GetRepository(), dgst); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "manifest not found")
		}

		return nil, status.Errorf(codes.Internal, "delete manifest: %v", err)
	}

	return &ociv1.DeleteManifestResponse{}, nil
}

// ListReferrers returns the same image index the HTTP endpoint serves.
//
// It marshals the index rather than re-modelling it in protobuf so that both
// surfaces are provably answering with the same bytes.
func (s *Server) ListReferrers(ctx context.Context, req *ociv1.ListReferrersRequest) (*ociv1.ListReferrersResponse, error) {
	subject, err := digest.Parse(req.GetSubject())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid subject digest")
	}

	page, err := s.reg.Referrers(ctx, req.GetRepository(), subject, req.GetArtifactType(), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list referrers: %v", err)
	}

	body, err := json.Marshal(page.Index)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode index: %v", err)
	}

	return &ociv1.ListReferrersResponse{
		Index:                     body,
		ArtifactTypeFilterApplied: req.GetArtifactType() != "",
	}, nil
}
