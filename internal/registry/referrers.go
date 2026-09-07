package registry

import (
	"context"
	"log/slog"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/model"
)

// Referrers lists the manifests in a repository that name subject, newest first.
//
// This is the query the whole metastore exists for. A referrer stores the pointer
// on itself -- a signature knows what it signs -- but every client asks in the
// opposite direction: given an image, what has been said about it? Content
// addressing cannot answer that, because the answer is not derivable from the
// subject's bytes and changes every time someone signs it. Something has to keep
// an index from subject back to the manifests naming it, and that index is a table.
//
// artifactType narrows the result, and an empty string means no filter. The two
// are distinguishable because the empty string is not a legal artifact type.
//
// Two things are deliberately not errors here:
//
// A subject that does not exist. "What refers to this" is answerable without the
// target being present, and a referrer often arrives first -- signing tools push
// the signature before, or entirely without, the image being in this registry.
//
// A subject with no referrers. An empty list means "nothing has been said about
// this", which is a fact, not a failure. Reporting it as one would be indistinct
// from a broken URL to the client, and a verification tool cannot afford to
// confuse "unsigned" with "I could not ask".
//
// A repository that does not exist *is* an error, which is the one place this
// reading of the spec is a judgement rather than a transcription. See the note in
// the HTTP handler.
func (r *Registry) Referrers(
	ctx context.Context,
	repository string,
	subject digest.Digest,
	artifactType string,
) ([]model.Manifest, error) {
	referrers, err := r.meta.Referrers(ctx, repository, subject, artifactType)
	if err != nil {
		return nil, err
	}

	// Only an empty result puts existence in question, so a repository with
	// referrers pays nothing for this. Note this cannot be folded into the query
	// above: "no rows" and "no such repository" are the same result there, which is
	// exactly the distinction being drawn.
	if len(referrers) == 0 {
		exists, existsErr := r.meta.RepositoryExists(ctx, repository)
		if existsErr != nil {
			return nil, existsErr
		}

		if !exists {
			return nil, ErrNotFound
		}
	}

	r.log.DebugContext(ctx, "referrers listed",
		slog.String("repository", repository),
		slog.String("subject", subject.String()),
		slog.String("artifactType", artifactType),
		slog.Int("count", len(referrers)),
	)

	return referrers, nil
}
