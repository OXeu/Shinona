// Package settings defines instance settings and account model settings.
package settings

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

type Key string

const (
	OAuthIssuer        Key = "oauthIssuer"
	OAuthClientID      Key = "oauthClientId"
	OAuthSecret        Key = "oauthClientSecret"
	MailAPIKey         Key = "mailApiKey"
	MailFrom           Key = "mailFrom"
	ModelBaseURL       Key = "modelBaseUrl"
	MCPServers         Key = "mcpServers"
	ModelID            Key = "modelId"
	ModelAPIKey        Key = "modelApiKey"
	E2BAPIKey          Key = "e2bApiKey"
	E2BTemplate        Key = "e2bTemplate"
	E2BBaseURL         Key = "e2bBaseUrl"
	E2BProxyURL        Key = "e2bProxyUrl"
	E2BDomain          Key = "e2bDomain"
	E2BTimeout         Key = "e2bTimeout"
	E2BOnTimeout       Key = "e2bOnTimeout"
	E2BAPIVersion      Key = "e2bApiVersion"
	E2BWorkspaceUser   Key = "e2bWorkspaceUser"
	E2BWorkDirectory   Key = "e2bWorkDirectory"
	SSHAddress         Key = "sshAddress"
	SSHUser            Key = "sshUser"
	SSHKeyPath         Key = "sshKeyPath"
	SSHHostFingerprint Key = "sshHostFingerprint"
	SSHTemplate        Key = "sshTemplate"
)

const (
	DefaultE2BTemplate       = "base"
	DefaultE2BTimeoutSeconds = 300
	DefaultSSHImage          = "buildpack-deps:bookworm-scm"
)

type definition struct {
	key      Key
	env      string
	fallback string
	alias    string
	secret   bool
	model    bool
}

var definitions = []definition{
	{key: OAuthIssuer, env: "SHINONA_OAUTH_ISSUER"},
	{key: OAuthClientID, env: "SHINONA_OAUTH_CLIENT_ID"},
	{key: OAuthSecret, env: "SHINONA_OAUTH_CLIENT_SECRET", secret: true},
	{key: MailAPIKey, env: "SHINONA_RESEND_API_KEY", secret: true},
	{key: MailFrom, env: "SHINONA_RESEND_FROM"},
	{key: ModelBaseURL, model: true},
	{key: MCPServers, secret: true},
	{key: ModelID, model: true},
	{key: ModelAPIKey, model: true, secret: true},
	{key: E2BAPIKey, env: "E2B_API_KEY", secret: true},
	{key: E2BTemplate, env: "SHINONA_E2B_TEMPLATE", fallback: DefaultE2BTemplate},
	{key: E2BBaseURL, env: "SHINONA_E2B_BASE_URL", alias: "E2B_API_URL"},
	{key: E2BProxyURL, env: "SHINONA_E2B_PROXY_URL", alias: "E2B_SANDBOX_URL"},
	{key: E2BDomain, env: "SHINONA_E2B_DOMAIN", alias: "E2B_DOMAIN"},
	{key: E2BTimeout, env: "SHINONA_E2B_TIMEOUT", fallback: strconv.Itoa(DefaultE2BTimeoutSeconds)},
	{key: E2BOnTimeout, env: "SHINONA_E2B_ON_TIMEOUT", fallback: "pause"},
	{key: E2BAPIVersion, env: "SHINONA_E2B_API_VERSION", fallback: "auto"},
	{key: E2BWorkspaceUser, env: "SHINONA_E2B_WORKSPACE_USER"},
	{key: E2BWorkDirectory, env: "SHINONA_E2B_WORK_DIRECTORY"},
	{key: SSHAddress, env: "SHINONA_SSH_ADDRESS"},
	{key: SSHUser, env: "SHINONA_SSH_USER"},
	{key: SSHKeyPath, env: "SHINONA_SSH_KEY_PATH"},
	{key: SSHHostFingerprint, env: "SHINONA_SSH_HOST_FINGERPRINT"},
	{key: SSHTemplate, env: "SHINONA_SSH_TEMPLATE", fallback: DefaultSSHImage},
}

type Values map[Key]string

type Repository interface {
	SettingOverrides(context.Context) (Values, error)
	SaveSettingOverrides(context.Context, Values) error
}

type ModelRepository interface {
	ModelSettings(context.Context, string) (Values, error)
	SaveModelSettings(context.Context, string, Values) error
}

// Defaults resolves startup inputs once. Saved values are merged over this set.
func Defaults(lookup func(string) (string, bool)) Values {
	values := make(Values, len(definitions))
	for _, field := range definitions {
		if field.model || field.key == MCPServers {
			continue
		}
		value, ok := lookup(field.env)
		if !ok && field.alias != "" {
			value, ok = lookup(field.alias)
		}
		if !ok {
			value = field.fallback
		}
		values[field.key] = value
	}
	return values
}

func Effective(defaults, overrides Values) Values {
	values := maps.Clone(defaults)
	for key, value := range overrides {
		if _, ok := defaults[key]; ok {
			values[key] = value
		}
	}
	return values
}

type Change struct {
	Key   Key
	Value *string // nil restores the startup default
}

var ErrInvalid = errors.New("invalid settings")

type ValidationError struct {
	Code    string
	message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", ErrInvalid, e.message) }

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func Invalid(code, message string) error { return &ValidationError{Code: code, message: message} }

func Apply(overrides Values, changes []Change) (Values, error) {
	return apply(overrides, changes, false)
}

func ApplyModel(overrides Values, changes []Change) (Values, error) {
	return apply(overrides, changes, true)
}

func apply(overrides Values, changes []Change, model bool) (Values, error) {
	if len(changes) > len(definitions) {
		return nil, Invalid("INVALID_SETTING_CHANGES", "too many changes")
	}
	next := make(Values, len(overrides)+len(changes))
	for key, value := range overrides {
		// Retain legacy model values in the database for operator recovery.
		// Instance defaults and fields exclude them.
		if field, ok := definitionFor(key); ok && (!model || field.model) {
			next[key] = value
		}
	}
	seen := make(map[Key]bool, len(changes))
	for _, change := range changes {
		field, known := definitionFor(change.Key)
		if seen[change.Key] || !known || field.model != model || change.Key == MCPServers {
			return nil, Invalid("INVALID_SETTING_CHANGES", "unknown or repeated setting")
		}
		seen[change.Key] = true
		if change.Value == nil {
			delete(next, change.Key)
			continue
		}
		if len(*change.Value) > 4096 || strings.ContainsRune(*change.Value, 0) {
			return nil, Invalid("INVALID_SETTING_VALUE", "setting value is too long or contains a null character")
		}
		next[change.Key] = strings.TrimSpace(*change.Value)
	}
	return next, nil
}

func definitionFor(key Key) (definition, bool) {
	for _, definition := range definitions {
		if definition.key == key {
			return definition, true
		}
	}
	return definition{}, false
}

type Field struct {
	Key        string  `json:"key"`
	Value      *string `json:"value"`
	Configured bool    `json:"configured"`
	Overridden bool    `json:"overridden"`
}

func Fields(values, overrides Values) []Field {
	return fields(values, overrides, false)
}

func ModelFields(values Values) []Field {
	return fields(values, values, true)
}

func fields(values, overrides Values, model bool) []Field {
	result := make([]Field, 0, len(definitions))
	for _, definition := range definitions {
		if definition.model != model || definition.key == MCPServers {
			continue
		}
		value := values[definition.key]
		_, overridden := overrides[definition.key]
		field := Field{Key: string(definition.key), Configured: value != "", Overridden: overridden}
		if !definition.secret {
			field.Value = &value
		}
		result = append(result, field)
	}
	return result
}
