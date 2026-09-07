package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/opencontainers/go-digest"

	"github.com/tkircsi/cairn/internal/metastore"
	"github.com/tkircsi/cairn/internal/model"
)

// tagRE is the tag grammar from the spec: a leading alphanumeric or underscore,
// then up to 127 more of those plus dot and hyphen.
//
// Anchored at both ends, which is the whole point -- an unanchored match would
// accept a tag with a slash or a colon in it, and a tag becomes part of a URL
// path and a metastore key.
var tagRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// ValidTag reports whether a reference is a well-formed tag.
//
// Exported because the HTTP layer needs the same grammar for a different reason:
// this package uses it to refuse writing a bad tag, and the handler uses it to
// tell a tag apart from a digest so that a client using one gets a sensible
// answer rather than a parse error.
func ValidTag(name string) bool { return tagRE.MatchString(name) }

// TagManifest points a tag at a manifest that this repository already holds.
//
// Requiring the manifest first is what keeps a tag from being a dangling name.
// The spec's own wording assumes it: a tag is pushed as part of a manifest PUT,
// so the manifest is present by construction, and there is no endpoint that
// creates a tag on its own.
func (r *Registry) TagManifest(
	ctx context.Context,
	repository, name string,
	dgst digest.Digest,
) (model.Tag, error) {
	if !ValidTag(name) {
		return model.Tag{}, fmt.Errorf("%w: %q is not a valid tag", ErrTagInvalid, name)
	}

	// Checked rather than assumed, because a tag may be pushed by digest via end-7b
	// against a manifest the client believes is there. A tag resolving to nothing
	// would be a 404 that looks like corruption.
	if _, err := r.meta.Manifest(ctx, repository, dgst); err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Tag{}, ErrNotFound
		}

		return model.Tag{}, err
	}

	tag := model.Tag{
		Repository: repository,
		Name:       name,
		Digest:     dgst,
		UpdatedAt:  r.now(),
	}

	if err := r.meta.PutTag(ctx, tag); err != nil {
		return model.Tag{}, err
	}

	// Logged at info because it is the one write that destroys information: the
	// manifest a tag used to point at is not recoverable from the store afterwards,
	// so if anyone asks later why "latest" moved, this line is the only answer.
	r.log.InfoContext(ctx, "tag set",
		slog.String("repository", repository),
		slog.String("tag", name),
		slog.String("digest", dgst.String()),
	)

	return tag, nil
}

// ResolveTag returns what a tag currently points at.
func (r *Registry) ResolveTag(ctx context.Context, repository, name string) (model.Tag, error) {
	tag, err := r.meta.Tag(ctx, repository, name)
	if err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return model.Tag{}, ErrNotFound
		}

		return model.Tag{}, err
	}

	return tag, nil
}

// Tags lists a page of a repository's tags.
//
// A negative limit means all of them; zero means none, which is a request the API
// can make and which must not be answered with everything.
//
// more reports whether a further page exists. It is derived by asking for one row
// past the limit rather than by counting, which costs a row instead of a second
// query and cannot disagree with the page it describes.
//
// A repository holding nothing is ErrNotFound rather than an empty list. That
// distinction is the spec's -- a pull of a repository that does not exist must
// 404 -- and it matters most for a typo, which would otherwise look like a
// repository that merely has no tags.
func (r *Registry) Tags(
	ctx context.Context,
	repository, after string,
	limit int,
) (names []string, more bool, err error) {
	if after != "" && !ValidTag(after) {
		return nil, false, fmt.Errorf("%w: %q is not a valid tag", ErrTagInvalid, after)
	}

	// One past the limit, so that "is there another page" is answered by the same
	// read. Left alone when unlimited (nothing follows everything) and when zero,
	// where fetching one would make an empty page look like it has a successor.
	fetch := limit
	if limit > 0 {
		fetch = limit + 1
	}

	names, err = r.meta.Tags(ctx, repository, after, fetch)
	if err != nil {
		return nil, false, err
	}

	if limit > 0 && len(names) > limit {
		names, more = names[:limit], true
	}

	// Only when the page is empty, and only then, is existence in question: a tag
	// is proof the repository exists, so the common path pays nothing.
	//
	// Note this is asked about a *page*, so an empty final page of a paginated walk
	// also triggers it -- which is correct but wasteful, and is why the check is a
	// single EXISTS rather than a count.
	if len(names) == 0 {
		exists, existsErr := r.meta.RepositoryExists(ctx, repository)
		if existsErr != nil {
			return nil, false, existsErr
		}

		if !exists {
			return nil, false, ErrNotFound
		}
	}

	return names, more, nil
}

// DeleteTag removes a name, leaving the manifest it pointed at in place.
//
// This is end-9 applied to a tag, and it is a different operation from end-9
// applied to a digest even though they share a URL: one forgets a name, the other
// forgets content and takes every name for it along too.
func (r *Registry) DeleteTag(ctx context.Context, repository, name string) error {
	if err := r.meta.DeleteTag(ctx, repository, name); err != nil {
		if errors.Is(err, metastore.ErrNotFound) {
			return ErrNotFound
		}

		return err
	}

	r.log.InfoContext(ctx, "tag deleted",
		slog.String("repository", repository),
		slog.String("tag", name),
	)

	return nil
}
