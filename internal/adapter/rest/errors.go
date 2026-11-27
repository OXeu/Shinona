package rest

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

type codedError struct {
	error
	code string
}

func (r *handler) apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, run.ErrInvalidPrompt):
		return codedError{err, "INVALID_PROMPT"}
	case errors.Is(err, run.ErrImages):
		return codedError{err, "INVALID_IMAGES"}
	case errors.Is(err, run.ErrTranscriptExportLimit):
		return codedError{err, "TRANSCRIPT_EXPORT_LIMIT"}
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
	case errors.Is(err, run.ErrProjectSandboxCleanup):
		return codedError{err, "PROJECT_SANDBOX_CLEANUP_FAILED"}
	case errors.Is(err, run.ErrProjectCleanupRequired):
		return codedError{err, "PROJECT_SANDBOX_CLEANUP_REQUIRED"}
	case errors.Is(err, sandbox.ErrProjectChanged):
		return codedError{err, "PROJECT_SANDBOX_CHANGED"}
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
		r.logger.Error("REST request failed", "error", err)
		return errors.New("the run service is unavailable")
	}
}

func writeError(c *gin.Context, err error) {
	var failure codedError
	if !errors.As(err, &failure) {
		reject(c, http.StatusInternalServerError, "SERVICE_UNAVAILABLE", err.Error())
		return
	}
	status := http.StatusBadRequest
	switch failure.code {
	case "SESSION_NOT_FOUND", "PROJECT_NOT_FOUND", "SSH_SERVER_NOT_FOUND", "MCP_SERVER_NOT_FOUND":
		status = http.StatusNotFound
	case "ADMIN_REQUIRED":
		status = http.StatusForbidden
	case "TRANSCRIPT_EXPORT_LIMIT":
		status = http.StatusRequestEntityTooLarge
	case "RUN_NOT_ACCEPTING", "QUEUE_FULL", "PROJECT_NAME_EXISTS", "SSH_SERVER_NAME_EXISTS", "SSH_SERVER_IN_USE",
		"WORKSPACE_BUSY", "WORKSPACE_SELECTION_CHANGED", "ACTIVE_TURN", "HISTORY_CHANGED", "RETRY_TURN_UNAVAILABLE",
		"PROJECT_SANDBOX_CLEANUP_REQUIRED", "PROJECT_SANDBOX_CHANGED":
		status = http.StatusConflict
	case "SANDBOX_PROVIDER_UNAVAILABLE", "WORKSPACE_UNAVAILABLE":
		status = http.StatusServiceUnavailable
	case "MODEL_FETCH_FAILED", "PROJECT_SANDBOX_CLEANUP_FAILED", "OAUTH_DISCOVERY_FAILED",
		"SSH_CONNECTION_REFUSED", "SSH_HOST_UNREACHABLE", "SSH_CONNECTION_FAILED":
		status = http.StatusBadGateway
	case "SSH_CONNECTION_TIMEOUT", "SSH_DOCKER_TIMEOUT":
		status = http.StatusGatewayTimeout
	}
	reject(c, status, failure.code, failure.Error())
}

func reject(c *gin.Context, status int, code, message string) {
	c.Abort()
	httpserver.WriteError(c.Writer, status, code, message)
}
