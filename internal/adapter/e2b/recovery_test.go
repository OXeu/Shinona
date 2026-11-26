package e2b

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestProjectSandboxRecovery(t *testing.T) {
	const (
		oldID   = "previous-box"
		newID   = "replacement-box"
		backup  = "snapshot-a:default"
		expired = "expired-snapshot:default"
	)
	cases := []struct {
		name           string
		connectStatus  int
		listStatus     int
		snapshots      []string
		previousBackup string
		restoreStatus  int
		cloud          bool
		wantTemplates  []string
	}{
		{
			name: "restore a saved cloud snapshot", cloud: true,
			connectStatus: http.StatusNotFound, listStatus: http.StatusOK,
			snapshots: []string{expired, backup}, restoreStatus: http.StatusCreated,
			wantTemplates: []string{expired, backup},
		},
		{
			name:          "recreate self-hosted sandbox with no snapshot API",
			connectStatus: http.StatusNotFound, listStatus: http.StatusNotFound,
			wantTemplates: []string{"base"},
		},
		{
			name:          "restore prior lineage if the replacement also disappears",
			connectStatus: http.StatusGone, listStatus: http.StatusOK, previousBackup: backup,
			restoreStatus: http.StatusCreated, wantTemplates: []string{backup},
		},
		{
			name:          "recreate when all known snapshots are gone",
			connectStatus: http.StatusNotFound, listStatus: http.StatusOK,
			snapshots: []string{expired}, previousBackup: expired,
			wantTemplates: []string{expired, "base"},
		},
		{
			name:          "preserve binding on reconnect outage",
			connectStatus: http.StatusServiceUnavailable,
		},
		{
			name:          "preserve binding on reconnect permission error",
			connectStatus: http.StatusForbidden,
		},
		{
			name:          "preserve binding on snapshot listing outage",
			connectStatus: http.StatusNotFound, listStatus: http.StatusServiceUnavailable,
		},
		{
			name:          "preserve binding on snapshot restore outage",
			connectStatus: http.StatusNotFound, listStatus: http.StatusOK,
			snapshots: []string{backup}, restoreStatus: http.StatusServiceUnavailable,
			wantTemplates: []string{backup},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var mu sync.Mutex
			var templates, keys []string
			box := func(w http.ResponseWriter, status int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"sandboxID":"replacement-box","envdVersion":"0.11.0","envdAccessToken":"replacement-token"}`)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/files" && r.Header.Get("X-API-Key") != "test-key" {
					t.Error("missing API credential")
				}
				switch r.URL.Path {
				case "/sandboxes/" + oldID + "/connect", "/v2/sandboxes/" + oldID + "/connect":
					w.WriteHeader(tc.connectStatus)
				case "/sandboxes/" + newID + "/connect", "/v2/sandboxes/" + newID + "/connect":
					box(w, http.StatusOK)
				case "/snapshots":
					if r.URL.Query().Get("sandboxID") != oldID || tc.listStatus == 0 {
						t.Error("unexpected snapshot discovery")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.listStatus)
					if tc.listStatus == http.StatusOK {
						list := make([]map[string]string, 0, len(tc.snapshots))
						for _, id := range tc.snapshots {
							list = append(list, map[string]string{"snapshotID": id})
						}
						_ = json.NewEncoder(w).Encode(list)
					}
				case "/sandboxes", "/v2/sandboxes":
					var input struct{ TemplateID string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					templates = append(templates, input.TemplateID)
					keys = append(keys, r.Header.Get("Idempotency-Key"))
					mu.Unlock()
					switch input.TemplateID {
					case expired:
						w.WriteHeader(http.StatusNotFound)
					case backup:
						box(w, tc.restoreStatus)
					case "base":
						box(w, http.StatusCreated)
					default:
						t.Errorf("unexpected template %s", input.TemplateID)
						w.WriteHeader(http.StatusBadRequest)
					}
				case "/files":
					if r.Header.Get("X-Access-Token") != "replacement-token" || r.Header.Get("E2b-Sandbox-Id") != newID {
						t.Error("the replacement did not use its own credentials")
					}
					_, _ = io.WriteString(w, "reconnected")
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			store := openStore(t, filepath.Join(t.TempDir(), "shinona.db"))
			config := Config{APIKey: "test-key", Template: "base", BaseURL: server.URL, ProxyURL: server.URL, APIVersion: "v1", WorkDirectory: "/workspace"}
			client := server.Client()
			if tc.cloud {
				config.BaseURL = ""
				config.APIVersion = "v2"
				client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					req := r.Clone(r.Context())
					req.URL.Scheme, req.URL.Host = "http", server.Listener.Addr().String()
					return http.DefaultTransport.RoundTrip(req)
				})}
			}
			provider, err := New(config, client, store)
			if err != nil {
				t.Fatal(err)
			}
			spec := sandbox.Spec{RunID: "run-id", OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
			seedWorkspaceOwner(t, store, spec.OwnerID, spec.ProjectID)
			if saved, err := provider.saveProject(ctx, spec, "", oldID,
				credentials{EnvdToken: "old-token", RecoverySnapshot: tc.previousBackup}); err != nil || !saved {
				t.Fatalf("seed saved binding: %v, %v", saved, err)
			}
			if tc.connectStatus == http.StatusNotFound || tc.connectStatus == http.StatusGone {
				if _, err := provider.OpenProject(ctx, spec); !errors.Is(err, sandbox.ErrWorkspaceMissing) {
					t.Fatalf("missing sandbox should leave inspection unavailable: %v", err)
				}
			}
			workspace, err := provider.Acquire(ctx, spec)
			succeeded := len(tc.wantTemplates) > 0 && tc.restoreStatus != http.StatusServiceUnavailable
			if succeeded != (err == nil) {
				t.Fatalf("unexpected acquisition: %v", err)
			}
			project, err := provider.loadProject(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotTemplates, gotKeys := slices.Clone(templates), slices.Clone(keys)
			mu.Unlock()
			// The SDK retries transient control-plane failures on the same image.
			if !slices.Equal(slices.Compact(gotTemplates), tc.wantTemplates) {
				t.Fatalf("creation attempts %v, want %v", gotTemplates, tc.wantTemplates)
			}
			if tc.cloud && (gotKeys[0] == "" || gotKeys[1] == "" || gotKeys[0] == gotKeys[1]) {
				t.Fatal("different cloud restore candidates need distinct idempotency keys")
			}
			if !succeeded {
				if project.id != oldID || project.tokens.EnvdToken != "old-token" {
					t.Fatal("a failed recovery modified the saved binding")
				}
				return
			}
			wantSnapshot := ""
			if tc.wantTemplates[len(tc.wantTemplates)-1] == backup {
				wantSnapshot = backup
			}
			if project.id != newID || project.tokens.RecoverySnapshot != wantSnapshot {
				t.Fatalf("replacement binding did not preserve the restoration source: %+v", project)
			}
			if err := workspace.Close(ctx); err != nil {
				t.Fatal(err)
			}
			reloaded, err := New(config, client, store)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := reloaded.OpenProject(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			data, err := reopened.ReadFile(ctx, "test.txt")
			if err != nil || string(data) != "reconnected" {
				t.Fatalf("read replacement after restart: %q, %v", data, err)
			}
		})
	}
}
