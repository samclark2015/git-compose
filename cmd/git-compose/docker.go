package main

import (
	"context"
	"fmt"

	"git-compose/internal/ui"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// ensureNetwork creates the named Docker bridge network if it doesn't exist.
func ensureNetwork(name string) error {
	ui.Step("Ensuring %s network", name)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	ctx := context.Background()
	_, err = cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		ui.Info("%s network already exists", name)
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("network inspect: %w", err)
	}

	_, err = cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		ui.Warn("failed to create %s network", name)
		return err
	}
	ui.OK("%s network created", name)
	return nil
}

// pruneImages removes dangling Docker images (equivalent to docker image prune -f).
func pruneImages() {
	ui.Step("Pruning dangling images")

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		ui.Warn("docker client for prune: %v", err)
		return
	}
	defer cli.Close()

	report, err := cli.ImagesPrune(context.Background(), filters.Args{})
	if err != nil {
		ui.Warn("image prune: %v", err)
		return
	}
	ui.OK("reclaimed %d bytes across %d image(s)", report.SpaceReclaimed, len(report.ImagesDeleted))
}
