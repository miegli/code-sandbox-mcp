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
	if !ok || destDir == "" {
		// Default: use the name of the source directory
		destDir = filepath.Join("/app", filepath.Base(localSrcDir))
	} else {
		// If provided but doesn't start with /, prepend /app/
		if !strings.HasPrefix(destDir, "/") {
			destDir = filepath.Join("/app", destDir)
		}
	}

	// Create tar archive of the source directory
	tarBuffer, err := createTarArchive(localSrcDir)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error creating tar archive: %v", err)), nil
	}

	// CopyToContainer directly extracts the tar to the destination
	// We need to copy to the parent of destDir and let it create the final directory
	parentDir := filepath.Dir(destDir)
	err = copyToContainer(ctx, containerID, parentDir, tarBuffer)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error copying to container: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully copied %s to %s in container %s", localSrcDir, destDir, containerID)), nil
}

// createTarArchive creates a tar archive of the specified source path
func createTarArchive(srcPath string) (io.Reader, error) {
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

		header.Name = filepath.Join(baseDir, relPath)

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

	// Ensure the destination directory exists in the container
	createDirCmd := []string{"mkdir", "-p", destPath}
	if err := executeCommand(ctx, containerID, createDirCmd); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Copy the tar archive to the container - this automatically extracts it
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
