// Package graphql exposes the signed-in run service as a schema-first GraphQL API.
package graphql

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

//go:embed schema.graphql
var definition string

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
	RemoveSSHServer(context.Context, string) error
}

type resolver struct {
	service *run.Service
	app     Application
	logger  *slog.Logger
}

func NewHandler(service *run.Service, app Application, logger *slog.Logger) (http.Handler, error) {
	root := &resolver{service: service, app: app, logger: logger}
	schema, err := gql.ParseSchema(definition, root, gql.MaxDepth(6), gql.MaxParallelism(8), gql.UseFieldResolvers(), gql.Tracer(timingTracer{}))
	if err != nil {
		return nil, err
	}
	return httpserver.MaxBytesHandler(graphqlHTTP{schema: schema}, 6<<20), nil
}

func (r *resolver) Mode() string { return r.app.Mode() }

func (r *resolver) Sessions(ctx context.Context, args struct{ ProjectID *gql.ID }) ([]summaryResolver, error) {
	items, err := r.service.RecentSessions(ctx, auth.CurrentUser(ctx).ID, (*string)(args.ProjectID))
	if err != nil {
		return nil, r.apiError(err)
	}
	results := make([]summaryResolver, len(items))
	for i, item := range items {
		results[i] = summaryResolver{item}
	}
	return results, nil
}

func (r *resolver) Session(ctx context.Context, args struct{ ID gql.ID }) (*sessionResolver, error) {
	transcript, err := r.service.Transcript(ctx, auth.CurrentUser(ctx).ID, string(args.ID))
	if err != nil {
		return nil, r.apiError(err)
	}
	if transcript == nil {
		return nil, nil
	}
	return &sessionResolver{summaryResolver: summaryResolver{transcript.Session}, turns: transcript.Turns, lastOptions: transcript.LastOptions()}, nil
}

func (r *resolver) CreateSession(ctx context.Context, args struct{ ProjectID *gql.ID }) (summaryResolver, error) {
	var projectID string
	if args.ProjectID != nil {
		projectID = string(*args.ProjectID)
	}
	item, err := r.service.CreateSession(ctx, auth.CurrentUser(ctx).ID, projectID)
	if err != nil {
		return summaryResolver{}, r.apiError(err)
	}
	return summaryResolver{item}, nil
}

func (r *resolver) UpdateSession(ctx context.Context, args struct {
	ID     gql.ID
	Title  *string
	Pinned *bool
}) (summaryResolver, error) {
	item, err := r.service.UpdateSession(ctx, auth.CurrentUser(ctx).ID, string(args.ID), run.SessionChanges{
		Title: args.Title, Pinned: args.Pinned,
	})
	if err != nil {
		return summaryResolver{}, r.apiError(err)
	}
	return summaryResolver{item}, nil
}

func (r *resolver) ArchiveSession(ctx context.Context, args struct{ ID gql.ID }) (bool, error) {
	if err := r.service.ArchiveSession(ctx, auth.CurrentUser(ctx).ID, string(args.ID)); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}

func (r *resolver) MarkSessionRead(ctx context.Context, args struct {
	ID        gql.ID
	Through   int32
	ThroughID *gql.ID
}) (bool, error) {
	if err := r.service.MarkSessionRead(ctx, auth.CurrentUser(ctx).ID, string(args.ID), int(args.Through), value((*string)(args.ThroughID))); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}

func (r *resolver) SandboxSettings(ctx context.Context) (settingsResolver, error) {
	settings, err := r.service.Settings(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return settingsResolver{}, r.apiError(err)
	}
	return settingsResolver{settings}, nil
}

func (r *resolver) SetSandboxPolicy(ctx context.Context, args struct{ Policy policy }) (settingsResolver, error) {
	settings, err := r.service.SetPolicy(ctx, auth.CurrentUser(ctx).ID, sandbox.Policy(args.Policy))
	if err != nil {
		return settingsResolver{}, r.apiError(err)
	}
	return settingsResolver{settings}, nil
}

type codedError struct {
	error
	code string
}

func (e codedError) Extensions() map[string]any {
	return map[string]any{"code": e.code}
}

func (r *resolver) apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, run.ErrInvalidPrompt):
		return codedError{err, "INVALID_PROMPT"}
	case errors.Is(err, run.ErrImages):
		return codedError{err, "INVALID_IMAGES"}
	case errors.Is(err, run.ErrOptions):
		return codedError{err, "INVALID_COMPOSER_OPTIONS"}
	case errors.Is(err, run.ErrNotRunning):
		return codedError{err, "RUN_NOT_ACCEPTING"}
	case errors.Is(err, run.ErrQueueFull):
		return codedError{err, "QUEUE_FULL"}
	case errors.Is(err, run.ErrInvalidTitle):
		return codedError{err, "INVALID_SESSION_TITLE"}
	case errors.Is(err, run.ErrSession):
		return codedError{err, "SESSION_NOT_FOUND"}
	case errors.Is(err, run.ErrProjectName):
		return codedError{err, "INVALID_PROJECT_NAME"}
	case errors.Is(err, run.ErrProjectExists):
		return codedError{err, "PROJECT_NAME_EXISTS"}
	case errors.Is(err, run.ErrProject):
		return codedError{err, "PROJECT_NOT_FOUND"}
	case errors.Is(err, run.ErrProjectSandboxConfig):
		return codedError{err, "INVALID_PROJECT_SANDBOX"}
	case errors.Is(err, run.ErrSandboxProvider):
		return codedError{err, "SANDBOX_PROVIDER_UNAVAILABLE"}
	case errors.Is(err, settings.ErrSSHServer):
		return codedError{err, "SSH_SERVER_NOT_FOUND"}
	case errors.Is(err, settings.ErrSSHServerName):
		return codedError{err, "SSH_SERVER_NAME_EXISTS"}
	case errors.Is(err, settings.ErrSSHServerInUse):
		return codedError{err, "SSH_SERVER_IN_USE"}
	case errors.Is(err, run.ErrWorkspaceBusy):
		return codedError{err, "WORKSPACE_BUSY"}
	case errors.Is(err, run.ErrActiveRun):
		return codedError{err, "ACTIVE_TURN"}
	case errors.Is(err, run.ErrHistoryChanged):
		return codedError{err, "HISTORY_CHANGED"}
	case errors.Is(err, run.ErrRetryTurn):
		return codedError{err, "RETRY_TURN_UNAVAILABLE"}
	case errors.Is(err, run.ErrPolicy):
		return codedError{err, "SANDBOX_POLICY_UNAVAILABLE"}
	case errors.Is(err, run.ErrModelConfig):
		return codedError{err, "MISSING_MODEL_CONFIG"}
	default:
		r.logger.Error("GraphQL request failed", "error", err)
		return errors.New("the run service is unavailable")
	}
}

type summaryResolver struct{ session run.Session }

func (r summaryResolver) ID() gql.ID    { return gql.ID(r.session.ID) }
func (r summaryResolver) Title() string { return r.session.Title }
func (r summaryResolver) ProjectID() *gql.ID {
	if r.session.ProjectID == "" {
		return nil
	}
	id := gql.ID(r.session.ProjectID)
	return &id
}
func (r summaryResolver) DefaultTitle() bool {
	return r.session.TurnCount == 0 && !r.session.CustomTitle
}

func (r summaryResolver) TurnCount() int32  { return int32(r.session.TurnCount) }
func (r summaryResolver) UpdatedAt() string { return r.session.UpdatedAt.Format(time.RFC3339Nano) }
func (r summaryResolver) Pinned() bool      { return r.session.Pinned }
func (r summaryResolver) Active() bool      { return r.session.Active }
func (r summaryResolver) Unread() bool      { return r.session.Unread }

type sessionResolver struct {
	summaryResolver
	turns       []run.Run
	lastOptions *run.Options
}

func (r sessionResolver) LastOptions() *lastOptionsResolver {
	if r.lastOptions == nil {
		return nil
	}
	options := r.lastOptions
	return &lastOptionsResolver{Model: options.Model, Permission: options.Permission,
		Thinking: options.Thinking, MCPServers: append([]string{}, options.SelectedMCPServers()...), ArrowJS: options.ArrowJS}
}

type lastOptionsResolver struct {
	Model, Permission, Thinking string
	MCPServers                  []string
	ArrowJS                     bool
}

func (r sessionResolver) Turns() []runResolver {
	turns := make([]runResolver, len(r.turns))
	for i, item := range r.turns {
		turns[i] = runResolver{item}
	}
	return turns
}

type runResolver struct{ item run.Run }

func (r runResolver) ID() gql.ID      { return gql.ID(r.item.ID) }
func (r runResolver) Sequence() int32 { return int32(r.item.Sequence) }
func (r runResolver) Prompt() string  { return r.item.Prompt }
func (r runResolver) Status() status  { return status(r.item.Status) }
func (r runResolver) Policy() *policy { return optionalPolicy(r.item.Policy) }
func (r runResolver) Output() string  { return r.item.Output }
func (r runResolver) Parts() []run.TurnPart {
	return r.item.Parts
}
func (r runResolver) CreatedAt() string { return r.item.CreatedAt.Format(time.RFC3339Nano) }
func (r runResolver) StartedAt() *string {
	if r.item.StartedAt == nil {
		return nil
	}
	value := r.item.StartedAt.Format(time.RFC3339Nano)
	return &value
}
func (r runResolver) UpdatedAt() string { return r.item.UpdatedAt.Format(time.RFC3339Nano) }

func (r runResolver) ContextTokens() int32        { return int32(r.item.ContextTokens) }
func (r runResolver) ContextWindow() int32        { return int32(r.item.ContextWindow) }
func (r runResolver) TokenUsage() *run.TokenUsage { return r.item.TokenUsage }
func (r runResolver) LiveContext() *run.ContextUsage {
	if r.item.LiveContext.Tokens == 0 {
		return nil
	}
	return &r.item.LiveContext
}
func (r runResolver) Model() string { return r.item.Payload.Options.Model }

func (r runResolver) Attachments() []string {
	names := make([]string, 0, len(r.item.Payload.Images))
	for _, image := range r.item.Payload.Images {
		names = append(names, image.Name)
	}
	return names
}

func (r runResolver) Steering() []run.SteeringMessage {
	if r.item.Steering == nil {
		return []run.SteeringMessage{}
	}
	return r.item.Steering
}

type settingsResolver struct{ settings run.Settings }

func (r settingsResolver) Selected() *policy { return optionalPolicy(r.settings.Selected) }

func (r settingsResolver) Available() []policy {
	available := make([]policy, len(r.settings.Available))
	for i, choice := range r.settings.Available {
		available[i] = policy(choice)
	}
	return available
}

type status string

func (status) ImplementsGraphQLType(name string) bool { return name == "RunStatus" }

type policy string

func (policy) ImplementsGraphQLType(name string) bool { return name == "SandboxPolicy" }

func (p *policy) UnmarshalGraphQL(input any) error {
	value, ok := input.(string)
	if !ok || (value != string(sandbox.Shared) && value != string(sandbox.Fresh)) {
		return fmt.Errorf("invalid sandbox policy %v", input)
	}
	*p = policy(value)
	return nil
}

func optionalPolicy(value sandbox.Policy) *policy {
	if value == "" {
		return nil
	}
	return new(policy(value))
}

func (r *resolver) ApplicationSettings(ctx context.Context) []settings.Field {
	if !auth.CurrentUser(ctx).Admin {
		return []settings.Field{}
	}
	return r.app.Fields()
}

type settingChange struct {
	Key   string
	Value *string
}

func settingChanges(input []settingChange) []settings.Change {
	changes := make([]settings.Change, len(input))
	for i, change := range input {
		changes[i] = settings.Change{Key: settings.Key(change.Key), Value: change.Value}
	}
	return changes
}

func (r *resolver) UpdateApplicationSettings(ctx context.Context, args struct{ Changes []settingChange }) ([]settings.Field, error) {
	if !auth.CurrentUser(ctx).Admin {
		return nil, codedError{errors.New("administrator access is required"), "ADMIN_REQUIRED"}
	}
	fields, err := r.app.Update(ctx, settingChanges(args.Changes))
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return nil, codedError{err, invalid.Code}
	}
	if err != nil {
		r.logger.Error("save application settings", "error", err)
		return nil, errors.New("could not save application settings")
	}
	return fields, nil
}
