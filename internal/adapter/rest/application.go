// Package rest exposes the signed-in application API over HTTP.
package rest

import (
	"context"
	"log/slog"

	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
	"golang.org/x/sync/semaphore"
)

type Application interface {
	Mode() string
	Fields() []settings.Field
	Update(context.Context, []settings.Change) ([]settings.Field, error)
	ModelFields(context.Context) ([]settings.Field, error)
	UpdateModelSettings(context.Context, []settings.Change) ([]settings.Field, error)
	ComposerOptions(context.Context) (ComposerOptions, error)
	FetchModels(context.Context, *string, *string) ([]ModelInfo, error)
	MCPSettings(context.Context) (settings.MCPSettings, error)
	SaveMCPServer(context.Context, settings.MCPServerInput) (settings.MCPSettings, error)
	RemoveMCPServer(context.Context, string) (settings.MCPSettings, error)
	ResetMCPSettings(context.Context) (settings.MCPSettings, error)
	SSHServers(context.Context) ([]settings.SSHServer, error)
	SaveSSHServer(context.Context, settings.SSHServer) (settings.SSHServer, error)
	TestSSHServer(context.Context, settings.SSHServer) error
	RemoveSSHServer(context.Context, string) error
}

type handler struct {
	service         *run.Service
	app             Application
	logger          *slog.Logger
	fullTranscript  *semaphore.Weighted
	pagedTranscript *semaphore.Weighted
	transcriptTurns encodedTranscriptTurns
	transcriptPages *responsecache.Cache[transcriptPageKey, transcriptPageBody]
}
