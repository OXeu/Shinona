package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/semaphore"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

const Prefix = "/api/v1"

// Register installs resource routes on the authenticated API group.
func Register(api *gin.RouterGroup, service *run.Service, app Application, logger *slog.Logger) {
	readConnections := min(max(runtime.GOMAXPROCS(0), 2), 16)
	fullSlots := int64(min(4, readConnections-1))
	h := &handler{
		service: service, app: app, logger: logger,
		fullTranscript: semaphore.NewWeighted(fullSlots), pagedTranscript: semaphore.NewWeighted(int64(2 * max(4, readConnections))),
		transcriptPages: responsecache.New[transcriptPageKey, transcriptPageBody](256, 16<<20),
	}
	api.Use(protectWrites())
	api.GET("/mode", read(func(context.Context) (string, error) { return h.app.Mode(), nil }))
	api.GET("/sessions", h.sessionList())
	api.POST("/sessions", endpoint(http.StatusCreated, true, h.createSession))
	api.PATCH("/sessions/:id", endpoint(http.StatusOK, true, h.updateSession))
	api.DELETE("/sessions/:id", command(false, h.archiveSession))
	api.PUT("/sessions/:id/read-marker", command(true, h.markSessionRead))
	api.GET("/sessions/:id/transcript", h.transcript)
	api.POST("/sessions/:id/turns", endpoint(http.StatusAccepted, true, h.SubmitRun))
	api.POST("/sessions/:id/turns/:turnId/retry", endpoint(http.StatusAccepted, true, h.RetryTurn))
	api.POST("/sessions/:id/turns/:turnId/revert", endpoint(http.StatusOK, true, h.RevertTurn))
	api.POST("/sessions/:id/turns/:turnId/steering", command(true, h.SteerRun))
	api.POST("/sessions/:id/turns/:turnId/steering/:steeringId/queue", command(true, h.queueSteering))
	api.DELETE("/sessions/:id/turns/:turnId/steering/:steeringId", endpoint(http.StatusOK, false, h.takeSteering))
	api.POST("/sessions/:id/turns/:turnId/steer", command(true, h.steerQueued))
	api.DELETE("/sessions/:id/turns/:turnId/queue", endpoint(http.StatusOK, false, h.takeQueued))
	api.POST("/sessions/:id/turns/:turnId/stop", command(false, h.StopRun))

	api.GET("/sessions/:id/workspace/root", endpoint(http.StatusOK, false, h.WorkspaceRoot))
	api.PUT("/sessions/:id/workspace/root", endpoint(http.StatusOK, true, h.SetWorkspaceRoot))
	api.GET("/sessions/:id/workspace/changes", endpoint(http.StatusOK, false, h.WorkspaceChanges))
	api.GET("/sessions/:id/workspace/history", endpoint(http.StatusOK, false, h.WorkspaceHistory))
	api.GET("/sessions/:id/workspace/files", endpoint(http.StatusOK, false, h.WorkspaceDirectory))
	api.GET("/sessions/:id/workspace/preview", endpoint(http.StatusOK, false, h.WorkspacePreview))
	api.POST("/sessions/:id/workspace/commits", endpoint(http.StatusCreated, true, h.CommitWorkspace))

	api.GET("/projects", read(h.Projects))
	api.POST("/projects", endpoint(http.StatusCreated, true, h.CreateProject))
	api.PATCH("/projects/:id", endpoint(http.StatusOK, true, h.RenameProject))
	api.DELETE("/projects/:id", command(false, h.RemoveProject))
	api.GET("/projects/:id/sandbox", endpoint(http.StatusOK, false, h.ProjectSandbox))
	api.PUT("/projects/:id/sandbox/config", command(true, h.SaveProjectSandbox))
	api.POST("/projects/:id/sandbox", command(false, h.CreateProjectSandbox))
	api.DELETE("/projects/:id/sandbox", command(false, h.DestroyProjectSandbox))
	api.POST("/projects/:id/sandbox/recreate", command(false, h.RecreateProjectSandbox))

	api.GET("/composer/options", read(h.ComposerOptions))
	api.PUT("/composer/defaults", endpoint(http.StatusOK, true, h.SetComposerDefaults))
	api.GET("/settings", read(h.settingsOverview))
	api.GET("/settings/application", read(func(ctx context.Context) ([]settings.Field, error) {
		return h.ApplicationSettings(ctx), nil
	}))
	api.PATCH("/settings/application", endpoint(http.StatusOK, true, h.UpdateApplicationSettings))
	api.GET("/settings/model", read(h.ModelSettings))
	api.PATCH("/settings/model", endpoint(http.StatusOK, true, h.UpdateModelSettings))
	api.POST("/settings/model/catalog", endpoint(http.StatusOK, true, h.FetchModels))
	api.GET("/settings/sandbox", read(h.SandboxSettings))
	api.PUT("/settings/sandbox/policy", endpoint(http.StatusOK, true, h.setSandboxPolicy))
	api.GET("/settings/mcp", read(h.MCPSettings))
	api.DELETE("/settings/mcp", read(h.ResetMCPSettings))
	api.PUT("/settings/mcp/servers", endpoint(http.StatusOK, true, h.SaveMCPServer))
	api.DELETE("/settings/mcp/servers", endpoint(http.StatusOK, false, h.RemoveMCPServer))
	api.GET("/settings/ssh", read(h.SSHServers))
	api.PUT("/settings/ssh/servers", endpoint(http.StatusOK, true, h.SaveSSHServer))
	api.POST("/settings/ssh/connection-test", command(true, h.TestSSHServer))
	api.DELETE("/settings/ssh/servers/:id", command(false, h.RemoveSSHServer))
}

func protectWrites() gin.HandlerFunc {
	protection := http.NewCrossOriginProtection()
	return func(c *gin.Context) {
		if err := protection.Check(c.Request); err != nil {
			reject(c, http.StatusForbidden, "CROSS_ORIGIN_REQUEST", "Cross-origin request is forbidden.")
			return
		}
		c.Next()
	}
}

func read[Output any](operation func(context.Context) (Output, error)) gin.HandlerFunc {
	return endpoint(http.StatusOK, false, func(ctx context.Context, _ struct{}) (Output, error) {
		return operation(ctx)
	})
}

func endpoint[Input, Output any](status int, body bool, operation func(context.Context, Input) (Output, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		var args Input
		if !bind(c, &args, body) {
			return
		}
		result, err := operation(c.Request.Context(), args)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(status, result)
	}
}

func command[Input any](body bool, operation func(context.Context, Input) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		var args Input
		if !bind(c, &args, body) {
			return
		}
		if err := operation(c.Request.Context(), args); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func bind(c *gin.Context, args any, body bool) bool {
	if body {
		contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || contentType != "application/json" {
			reject(c, http.StatusUnsupportedMediaType, "JSON_REQUIRED", "An application/json body is required.")
			return false
		}
		decoder := json.NewDecoder(httpserver.MaxBytesReader(c.Writer, c.Request.Body, 6<<20))
		err = decoder.Decode(args)
		if err == nil {
			err = decoder.Decode(new(any))
			if err == nil {
				err = errors.New("multiple JSON values")
			} else if errors.Is(err, io.EOF) {
				err = nil
			}
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				reject(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "Request body is too large.")
			} else {
				reject(c, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON body.")
			}
			return false
		}
	} else if c.Request.URL.RawQuery != "" && c.ShouldBindQuery(args) != nil {
		reject(c, http.StatusBadRequest, "INVALID_QUERY", "Invalid query parameters.")
		return false
	}
	if len(c.Params) != 0 && c.ShouldBindUri(args) != nil {
		reject(c, http.StatusBadRequest, "INVALID_RESOURCE_PATH", "Invalid resource path.")
		return false
	}
	return true
}
