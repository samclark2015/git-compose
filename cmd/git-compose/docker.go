package main

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// ensureNetwork creates the named Docker bridge network if it doesn't exist.
func ensureNetwork(name string) error {
	step("Ensuring %s network", name)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	ctx := context.Background()
	_, err = cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		info("%s network already exists", name)
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("network inspect: %w", err)
	}

	_, err = cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		warn("failed to create %s network", name)
		return err
	}
	ok("%s network created", name)
	return nil
}

// pruneImages removes dangling Docker images (equivalent to docker image prune -f).
func pruneImages() {
	step("Pruning dangling images")

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		warn("docker client for prune: %v", err)
		return
	}
	defer cli.Close()

	report, err := cli.ImagesPrune(context.Background(), filters.Args{})
	if err != nil {
		warn("image prune: %v", err)
		return
	}
	ok("reclaimed %d bytes across %d image(s)", report.SpaceReclaimed, len(report.ImagesDeleted))
}
