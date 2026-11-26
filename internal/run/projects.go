package run

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

var (
	ErrProjectName   = errors.New("enter a project name between 1 and 80 characters")
	ErrProjectExists = errors.New("a project with this name already exists")
	ErrProject       = errors.New("project not found")
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
	if err := s.repo.CreateProject(ctx, project); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) RenameProject(ctx context.Context, owner, id, name string) (Project, error) {
	name, err := projectName(name)
	if err != nil {
		return Project{}, err
	}
	return s.repo.RenameProject(ctx, owner, id, name)
}

func (s *Service) RemoveProject(ctx context.Context, owner, id string) error {
	if !s.projectMu.TryLock() {
		return ErrWorkspaceBusy
	}
	defer s.projectMu.Unlock()
	err := s.repo.RemoveProject(ctx, owner, id)
	if err == nil {
		s.controls.changedAccount(owner)
	}
	return err
}

func (s *Service) Projects(ctx context.Context, owner string) ([]Project, error) {
	return s.repo.Projects(ctx, owner)
}

func projectName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return "", ErrProjectName
	}
	return name, nil
}
