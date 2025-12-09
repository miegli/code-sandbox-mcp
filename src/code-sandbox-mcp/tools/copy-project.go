package tools

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// CopyProject copies a local directory to a container's filesystem
func CopyProject(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	containerID, ok := request.Params.Arguments["container_id"].(string)
	if !ok || containerID == "" {
		return mcp.NewToolResultText("container_id is required"), nil
	}

	localSrcDir, ok := request.Params.Arguments["local_src_dir"].(string)
	if !ok || localSrcDir == "" {
		return mcp.NewToolResultText("local_src_dir is required"), nil
	}

	// Clean and validate the source path
	localSrcDir = filepath.Clean(localSrcDir)
	info, err := os.Stat(localSrcDir)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error accessing source directory: %v", err)), nil
	}

	if !info.IsDir() {
		return mcp.NewToolResultText("local_src_dir must be a directory"), nil
	}

	// Get the destination path (optional parameter)
	destDir, ok := request.Params.Arguments["dest_dir"].(string)
	copyToHomeDir := false

	if !ok || destDir == "" || destDir == "." {
		// Default: copy contents directly to /app directory in the container
		destDir = "/app"
		copyToHomeDir = true
	} else {
		// If provided but doesn't start with /, prepend /app/
		if !strings.HasPrefix(destDir, "/") {
			destDir = filepath.Join("/app", destDir)
		}
	}

	// Create tar archive of the source directory
	tarBuffer, err := createTarArchive(localSrcDir, copyToHomeDir)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error creating tar archive: %v", err)), nil
	}

	// CopyToContainer directly extracts the tar to the destination
	var targetPath string
	if copyToHomeDir {
		// Copy contents directly to destDir (home directory)
		targetPath = destDir
	} else {
		// We need to copy to the parent of destDir and let it create the final directory
		targetPath = filepath.Dir(destDir)
	}

	err = copyToContainer(ctx, containerID, targetPath, tarBuffer)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error copying to container: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully copied %s to %s in container %s", localSrcDir, destDir, containerID)), nil
}

// getContainerHomeDir gets the home directory of the user running in the container
func getContainerHomeDir(ctx context.Context, cli *client.Client, containerID string) (string, error) {
	// Create the exec configuration to get the home directory
	exec, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          []string{"sh", "-c", "echo $HOME"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create exec: %w", err)
	}

	// Attach to capture stdout/stderr
	attach, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attach.Close()

	// Read the output
	var stdout bytes.Buffer
	io.Copy(&stdout, attach.Reader)

	// Wait for the command to complete
	for {
		inspect, err := cli.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return "", fmt.Errorf("failed to inspect exec: %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return "", fmt.Errorf("command exited with code %d", inspect.ExitCode)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	homeDir := strings.TrimSpace(stdout.String())
	if homeDir == "" {
		// Fallback to /root if HOME is not set
		homeDir = "/root"
	}

	return homeDir, nil
}

// createTarArchive creates a tar archive of the specified source path
func createTarArchive(srcPath string, copyContentsOnly bool) (io.Reader, error) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	defer tw.Close()

	srcPath = filepath.Clean(srcPath)
	baseDir := filepath.Base(srcPath)

	err := filepath.Walk(srcPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Create tar header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		// Maintain directory structure relative to the source directory
		relPath, err := filepath.Rel(srcPath, file)
		if err != nil {
			return err
		}

		if relPath == "." {
			// Skip the root directory itself
			return nil
		}

		// If copyContentsOnly is true, don't include the base directory name
		if copyContentsOnly {
			header.Name = relPath
		} else {
			header.Name = filepath.Join(baseDir, relPath)
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// If it's a regular file, write its content
		if fi.Mode().IsRegular() {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return buf, nil
}

// copyToContainer copies a tar archive to a container
func copyToContainer(ctx context.Context, containerID string, destPath string, tarArchive io.Reader) error {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Make sure the container exists and is running
	_, err = cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// Copy the tar archive to the container - this automatically extracts it
	// Docker's CopyToContainer will create parent directories if needed
	err = cli.CopyToContainer(ctx, containerID, destPath, tarArchive, container.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("failed to copy to container: %w", err)
	}

	return nil
}

// executeCommand runs a command in a container and waits for it to complete
func executeCommand(ctx context.Context, containerID string, cmd []string) error {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Create the exec configuration
	exec, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create exec: %w", err)
	}

	// Attach to capture stdout/stderr
	attach, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attach.Close()

	// Read output in background
	var stdout, stderr bytes.Buffer
	go func() {
		io.Copy(&stdout, attach.Reader)
	}()

	// Wait for the command to complete
	for {
		inspect, err := cli.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect exec: %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				errMsg := stderr.String()
				if errMsg == "" {
					errMsg = stdout.String()
				}
				return fmt.Errorf("command exited with code %d: %s", inspect.ExitCode, errMsg)
			}
			break
		}
		// Small sleep to avoid hammering the Docker API
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}
