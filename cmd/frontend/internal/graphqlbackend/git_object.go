package graphqlbackend

import (
	"context"
	"errors"
	"sync"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/backend"
	"github.com/sourcegraph/sourcegraph/pkg/vcs/git"
)

type gitObjectID string

func (gitObjectID) ImplementsGraphQLType(name string) bool {
	return name == "GitObjectID"
}

func (id *gitObjectID) UnmarshalGraphQL(input interface{}) error {
	if input, ok := input.(string); ok && git.IsAbsoluteRevision(input) {
		*id = gitObjectID(input)
		return nil
	}
	return errors.New("GitObjectID: expected 40-character string (SHA-1 hash)")
}

type gitObject struct {
	repo *repositoryResolver
	oid  gitObjectID
}

func (o *gitObject) OID(ctx context.Context) (gitObjectID, error) { return o.oid, nil }
func (o *gitObject) AbbreviatedOID(ctx context.Context) (string, error) {
	return string(o.oid[:7]), nil
}
func (o *gitObject) Commit(ctx context.Context) (*gitCommitResolver, error) {
	return o.repo.Commit(ctx, &struct{ Rev string }{Rev: string(o.oid)})
}

type gitObjectResolver struct {
	repo    *repositoryResolver
	revspec string

	once sync.Once
	oid  gitObjectID
	err  error
}

func (o *gitObjectResolver) resolve(ctx context.Context) (gitObjectID, error) {
	o.once.Do(func() {
		resolvedRev, err := backend.Repos.ResolveRev(ctx, o.repo.repo, o.revspec)
		if err != nil {
			o.err = err
			return
		}
		o.oid = gitObjectID(resolvedRev)
	})
	return o.oid, o.err
}

func (o *gitObjectResolver) OID(ctx context.Context) (gitObjectID, error) {
	return o.resolve(ctx)
}

func (o *gitObjectResolver) AbbreviatedOID(ctx context.Context) (string, error) {
	oid, err := o.resolve(ctx)
	if err != nil {
		return "", err
	}
	return string(oid[:7]), nil
}

func (o *gitObjectResolver) Commit(ctx context.Context) (*gitCommitResolver, error) {
	oid, err := o.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return o.repo.Commit(ctx, &struct{ Rev string }{Rev: string(oid)})
}
