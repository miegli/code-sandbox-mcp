package tools

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// WriteFile writes a file to the container's filesystem
func WriteFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	containerID, ok := request.Params.Arguments["container_id"].(string)
	if !ok || containerID == "" {
		return mcp.NewToolResultText("container_id is required"), nil
	}

	fileName, ok := request.Params.Arguments["file_name"].(string)
	if !ok || fileName == "" {
		return mcp.NewToolResultText("file_name is required"), nil
	}

	fileContents, ok := request.Params.Arguments["file_contents"].(string)
	if !ok {
		return mcp.NewToolResultText("file_contents is required"), nil
	}

	// Determine the full path
	var fullPath string
	if strings.HasPrefix(fileName, "/") {
		// fileName is already an absolute path, use it as-is
		fullPath = fileName
	} else {
		// fileName is relative, resolve it against destDir
		destDir, ok := request.Params.Arguments["dest_dir"].(string)
		if !ok || destDir == "" {
			// Default: write to the working directory
			destDir = "/app"
		} else {
			// If provided but doesn't start with /, prepend /app/
			if !strings.HasPrefix(destDir, "/") {
				destDir = filepath.Join("/app", destDir)
			}
		}
		fullPath = filepath.Join(destDir, fileName)
	}

	// Extract directory from full path
	dirPath := filepath.Dir(fullPath)

	// Create the directory if it doesn't exist
	if err := ensureDirectoryExists(ctx, containerID, dirPath); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error creating directory: %v", err)), nil
	}

	// Write the file
	if err := writeFileToContainer(ctx, containerID, fullPath, fileContents); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error writing file: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully wrote file %s to container %s", fullPath, containerID)), nil
}

// ensureDirectoryExists creates a directory in the container if it doesn't already exist
func ensureDirectoryExists(ctx context.Context, containerID, dirPath string) error {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Create the directory if it doesn't exist
	cmd := []string{"mkdir", "-p", dirPath}
	exec, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: cmd,
	})
	if err != nil {
		return fmt.Errorf("failed to create exec for mkdir: %w", err)
	}

	if err := cli.ContainerExecStart(ctx, exec.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("failed to start exec for mkdir: %w", err)
	}

	return nil
}

// writeFileToContainer writes file contents to a file in the container
func writeFileToContainer(ctx context.Context, containerID, filePath, contents string) error {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Command to write the content to the specified file using cat
	cmd := []string{"sh", "-c", fmt.Sprintf("cat > %s", filePath)}

	// Create the exec configuration
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	// Create the exec instance
	execIDResp, err := cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec: %w", err)
	}

	// Attach to the exec instance
	resp, err := cli.ContainerExecAttach(ctx, execIDResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer resp.Close()

	// Write the content to the container's stdin
	_, err = io.Copy(resp.Conn, strings.NewReader(contents))
	if err != nil {
		return fmt.Errorf("failed to write content to container: %w", err)
	}
	resp.CloseWrite()

	// Wait for the command to complete
	for {
		inspect, err := cli.ContainerExecInspect(ctx, execIDResp.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect exec: %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("command exited with code %d", inspect.ExitCode)
			}
			break
		}
		// Small sleep to avoid hammering the Docker API
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}
