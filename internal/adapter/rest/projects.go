package rest

import (
	"context"

	"github.com/OXeu/shinona/internal/auth"
)

type projectResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r *handler) Projects(ctx context.Context) ([]projectResponse, error) {
	items, err := r.service.Projects(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, r.apiError(err)
	}
	projects := make([]projectResponse, len(items))
	for i, project := range items {
		projects[i] = projectResponse{ID: project.ID, Name: project.Name}
	}
	return projects, nil
}

func (r *handler) CreateProject(ctx context.Context, args struct{ Name string }) (projectResponse, error) {
	project, err := r.service.CreateProject(ctx, auth.CurrentUser(ctx).ID, args.Name)
	if err != nil {
		return projectResponse{}, r.apiError(err)
	}
	return projectResponse{ID: project.ID, Name: project.Name}, nil
}

func (r *handler) RenameProject(ctx context.Context, args struct {
	ID   string `uri:"id" json:"-"`
	Name string `json:"name"`
}) (projectResponse, error) {
	project, err := r.service.RenameProject(ctx, auth.CurrentUser(ctx).ID, args.ID, args.Name)
	if err != nil {
		return projectResponse{}, r.apiError(err)
	}
	return projectResponse{ID: project.ID, Name: project.Name}, nil
}

func (r *handler) RemoveProject(ctx context.Context, args resourceID) (bool, error) {
	if err := r.service.RemoveProject(ctx, auth.CurrentUser(ctx).ID, args.ID); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}
