package run

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/sandbox"
)

var (
	ErrProjectName            = errors.New("enter a project name between 1 and 80 characters")
	ErrProjectExists          = errors.New("a project with this name already exists")
	ErrProject                = errors.New("project not found")
	ErrProjectCleanupRequired = errors.New("restore the original E2B control plane to clean up the remaining sandbox before removing this project")
)

type Project struct {
	ID      string
	OwnerID string
	Name    string
}

func (s *Service) CreateProject(ctx context.Context, owner, name string) (Project, error) {
	name, err := projectName(name)
	if err != nil {
		return Project{}, err
	}
	project := Project{ID: s.newID(), OwnerID: owner, Name: name}
	if err := s.stores.Projects.CreateProject(ctx, project); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) RenameProject(ctx context.Context, owner, id, name string) (Project, error) {
	name, err := projectName(name)
	if err != nil {
		return Project{}, err
	}
	return s.stores.Projects.RenameProject(ctx, owner, id, name)
}

func (s *Service) RemoveProject(ctx context.Context, owner, id string) error {
	if id == "" {
		return ErrProject
	}
	return s.changeProjectSandbox(ctx, owner, id, func(ctx context.Context, config ProjectSandboxConfig, spec sandbox.Spec) error {
		if err := s.cleanPreviousSandbox(ctx, config.Provider, spec, nil); err != nil {
			return err
		}
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionWriteTimeout)
		defer cancel()
		if err := s.stores.Projects.RemoveProject(save, owner, id); err != nil {
			return err
		}
		s.controls.changedAccount(owner)
		return nil
	})
}

func (s *Service) Projects(ctx context.Context, owner string) ([]Project, error) {
	return s.stores.Projects.Projects(ctx, owner)
}

func projectName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return "", ErrProjectName
	}
	return name, nil
}
