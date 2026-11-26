package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	sdk "github.com/eric642/e2b-go-sdk"

	"github.com/OXeu/shinona/internal/sandbox"
)

type envdRouteKey struct{}

type envdRoute struct {
	source, target                *url.URL
	id, accessToken, trafficToken string
}

type sandboxTransport struct {
	control *http.Client
	data    http.RoundTripper
}

func sdkHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	control := *client
	if control.Timeout == 0 || control.Timeout > apiTimeout {
		control.Timeout = apiTimeout
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	control.CheckRedirect = noRedirect
	data := client.Transport
	if data == nil {
		data = http.DefaultTransport
	}
	return &http.Client{Transport: sandboxTransport{control: &control, data: data}, CheckRedirect: noRedirect}
}

func (t sandboxTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	route, ok := request.Context().Value(envdRouteKey{}).(envdRoute)
	if !ok {
		return t.control.Do(request)
	}
	from := strings.TrimRight(route.source.EscapedPath(), "/")
	pathSuffix, withinBase := strings.CutPrefix(request.URL.EscapedPath(), from+"/")
	if !withinBase || request.URL.Scheme != route.source.Scheme || request.URL.Host != route.source.Host {
		return nil, errors.New("unexpected E2B SDK data-plane destination")
	}
	forward := request.Clone(request.Context())
	forward.URL.Scheme, forward.URL.Host = route.target.Scheme, route.target.Host
	forward.Host = route.target.Host
	forward.URL.RawPath = strings.TrimRight(route.target.EscapedPath(), "/") + "/" + pathSuffix
	decoded, err := url.PathUnescape(forward.URL.RawPath)
	if err != nil {
		return nil, err
	}
	forward.URL.Path = decoded
	forward.Header.Set("E2b-Sandbox-Id", route.id)
	forward.Header.Set("E2b-Sandbox-Port", strconv.Itoa(envdPort))
	if route.trafficToken != "" {
		forward.Header.Set("E2b-Traffic-Access-Token", route.trafficToken)
	}
	if route.accessToken != "" {
		forward.Header.Set("X-Access-Token", route.accessToken)
	}
	return t.data.RoundTrip(forward)
}

func (p *Provider) dataPlane(ctx context.Context, box *sdk.Sandbox, tokens credentials, existing bool) (*projectWorkspace, credentials, error) {
	if tokens.EnvdToken == "" && tokens.TrafficToken == "" {
		return nil, tokens, errors.New("E2B returned neither an envd access token nor a private traffic token")
	}
	source := p.sdkProxyURL
	target := p.config.ProxyURL
	host := box.GetHost(envdPort)
	if source == "" {
		source = "https://" + host
	}
	if target == "" {
		target = p.dataScheme + "://" + host
	}
	expected, err := parseEndpoint(source)
	if err != nil {
		return nil, tokens, err
	}
	endpoint, err := parseEndpoint(target)
	if err != nil {
		return nil, tokens, err
	}

	// Existing records predate execution settings and used root's home.
	if existing && tokens.WorkDirectory == "" {
		tokens.WorkspaceUser, tokens.WorkDirectory = "root", "/root"
	}
	if user := p.config.WorkspaceUser; user != "" && user != tokens.WorkspaceUser {
		tokens.WorkspaceUser, tokens.WorkDirectory = user, ""
	}
	if dir := p.config.WorkDirectory; dir != "" {
		tokens.WorkDirectory = path.Clean(dir)
	}
	ws := &workspace{box: box, user: tokens.WorkspaceUser, directory: tokens.WorkDirectory,
		processCleanupTimeout: p.config.CleanupTimeout,
		route:                 envdRoute{source: expected, target: endpoint, id: box.ID, accessToken: tokens.EnvdToken, trafficToken: tokens.TrafficToken}}
	if ws.directory == "" {
		discovery, cancel := context.WithTimeout(ctx, apiTimeout)
		defer cancel()
		result, err := ws.Exec(discovery, sandbox.Command{Program: "sh", Args: []string{"-c", "pwd -P"}})
		if err != nil {
			return nil, tokens, fmt.Errorf("resolve E2B template working directory: %w", err)
		}
		dir := strings.TrimSuffix(result.Stdout, "\n")
		if result.ExitCode != 0 || result.Truncated || !path.IsAbs(dir) || strings.ContainsRune(dir, 0) {
			return nil, tokens, errors.New("E2B did not report an absolute working directory")
		}
		ws.directory = path.Clean(dir)
		tokens.WorkDirectory = ws.directory
	}
	return &projectWorkspace{Workspace: ws, terminal: ws, sandboxID: box.ID, refresh: box.RefreshTimeout}, tokens, nil
}
