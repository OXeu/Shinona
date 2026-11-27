package sshsandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"path"
	"strconv"
	"strings"

	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	containerPolicyLabel = "org.shinona.sandbox.policy"
	containerPolicy      = "1"
	containerHome        = "/.shinona-home"
	// Keep the container running with an integer duration even on small images.
	containerIdle = "while sleep 86400; do :; done; exit 1"
)

type ContainerConfig struct {
	UID, GID      int
	WorkDirectory string
	CPUs          int
	MemoryMiB     int
	Pids          int
}

func DefaultContainerConfig() ContainerConfig {
	return ContainerConfig{
		UID: 65534, GID: 65534, WorkDirectory: "/workspace",
		CPUs: 2, MemoryMiB: 4096, Pids: 512,
	}
}

func (c ContainerConfig) Validate() error {
	if c.UID <= 0 || c.GID <= 0 || c.CPUs <= 0 || c.MemoryMiB < 6 || c.Pids <= 0 {
		return errors.New("SSH container IDs, CPU and process limits must be positive, and memory must be at least 6 MiB")
	}
	dir := c.WorkDirectory
	if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/" ||
		dir == containerHome || strings.HasPrefix(dir, containerHome+"/") || strings.ContainsRune(dir, 0) {
		return errors.New("SSH container work directory must be an absolute directory outside its managed home")
	}
	return nil
}

func (c ContainerConfig) limits() []string {
	memory := strconv.Itoa(c.MemoryMiB) + "m"
	return []string{"--cpus", strconv.Itoa(c.CPUs), "--memory", memory, "--memory-swap", memory,
		"--pids-limit", strconv.Itoa(c.Pids)}
}

func (c ContainerConfig) createArgs(name, template string) []string {
	args := []string{"create", "--init", "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--name", name, "--label", containerPolicyLabel + "=" + containerPolicy,
		"--user", strconv.Itoa(c.UID) + ":" + strconv.Itoa(c.GID),
		"--workdir", c.WorkDirectory, "--env", "HOME=" + containerHome,
		"--entrypoint", "sh"}
	args = append(args, c.limits()...)
	return append(args, "--", template, "-c", containerIdle)
}

func (c ContainerConfig) directories() (*bytes.Reader, error) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, dir := range []string{c.WorkDirectory, containerHome} {
		header := &tar.Header{Name: strings.TrimPrefix(dir, "/"), Typeflag: tar.TypeDir,
			Uid: c.UID, Gid: c.GID, Mode: 0o700}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(archive.Bytes()), nil
}

func (p *Provider) ensureRunning(ctx context.Context, ws *workspace, spec sandbox.Spec) error {
	status, err := ws.containerStatus(ctx)
	if err != nil {
		return err
	}
	if status.State == sandbox.ProjectNone {
		template := spec.Template
		if template == "" {
			template = p.template
		}
		if err := ws.docker(ctx, p.container.createArgs(ws.name, template)...); err != nil {
			return err
		}
		status = dockerStatus{created: true, hardened: true, ProjectStatus: sandbox.ProjectStatus{State: sandbox.ProjectStopped}}
	}
	if !status.hardened {
		// Docker can update resource limits in place. The user and capabilities
		// take effect when a legacy container is explicitly recreated.
		if _, applied := p.legacyLimits.Get(status.ID); !applied {
			args := append([]string{"update"}, p.container.limits()...)
			if err := ws.docker(ctx, append(args, ws.name)...); err != nil {
				return err
			}
			p.legacyLimits.Put(status.ID, struct{}{}, len(status.ID))
		}
	}
	switch status.State {
	case sandbox.ProjectRunning:
		return nil
	case sandbox.ProjectPaused:
		return ws.docker(ctx, "unpause", ws.name)
	}
	if status.created && status.hardened {
		// A container left in 'created' can be repaired after a canceled
		// provisioning request. Only an already-started container has user data.
		directories, err := p.container.directories()
		if err != nil {
			return err
		}
		result, err := ws.command(ctx, directories, sandbox.DefaultOutputLimit, "cp", "--archive", "-", ws.name+":/")
		if err := commandError(result, err); err != nil {
			return err
		}
	}
	if err := ws.docker(ctx, "start", ws.name); err != nil {
		return err
	}
	running, err := ws.status(ctx)
	if err == nil && running.State != sandbox.ProjectRunning {
		err = errors.New("SSH sandbox image did not keep its container running")
	}
	return err
}
