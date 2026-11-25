package graphql

import (
	"context"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/auth"
)

type projectResolver struct {
	ID   gql.ID
	Name string
}

func (r *resolver) Projects(ctx context.Context) ([]projectResolver, error) {
	items, err := r.service.Projects(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, r.apiError(err)
	}
	projects := make([]projectResolver, len(items))
	for i, project := range items {
		projects[i] = projectResolver{ID: gql.ID(project.ID), Name: project.Name}
	}
	return projects, nil
}

func (r *resolver) CreateProject(ctx context.Context, args struct{ Name string }) (projectResolver, error) {
	project, err := r.service.CreateProject(ctx, auth.CurrentUser(ctx).ID, args.Name)
	if err != nil {
		return projectResolver{}, r.apiError(err)
	}
	return projectResolver{ID: gql.ID(project.ID), Name: project.Name}, nil
}

func (r *resolver) RenameProject(ctx context.Context, args struct {
	ID   gql.ID
	Name string
}) (projectResolver, error) {
	project, err := r.service.RenameProject(ctx, auth.CurrentUser(ctx).ID, string(args.ID), args.Name)
	if err != nil {
		return projectResolver{}, r.apiError(err)
	}
	return projectResolver{ID: gql.ID(project.ID), Name: project.Name}, nil
}

func (r *resolver) RemoveProject(ctx context.Context, args struct{ ID gql.ID }) (bool, error) {
	if err := r.service.RemoveProject(ctx, auth.CurrentUser(ctx).ID, string(args.ID)); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}
