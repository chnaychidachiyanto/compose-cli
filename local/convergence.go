// +build local

/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package local

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/types"
	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose-cli/api/containers"
	"github.com/docker/compose-cli/progress"
)

const (
	extLifecycle  = "x-lifecycle"
	forceRecreate = "force_recreate"
)

func (s *local) ensureService(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	err := s.waitDependencies(ctx, project, service)
	if err != nil {
		return err
	}

	actual, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("%s=%s", projectLabel, project.Name)),
			filters.Arg("label", fmt.Sprintf("%s=%s", serviceLabel, service.Name)),
		),
	})
	if err != nil {
		return err
	}

	scale := getScale(service)

	eg, ctx := errgroup.WithContext(ctx)
	if len(actual) < scale {
		next, err := nextContainerNumber(actual)
		if err != nil {
			return err
		}
		missing := scale - len(actual)
		for i := 0; i < missing; i++ {
			number := next + i
			name := fmt.Sprintf("%s_%s_%d", project.Name, service.Name, number)
			eg.Go(func() error {
				return s.createContainer(ctx, project, service, name, number)
			})
		}
	}

	if len(actual) > scale {
		for i := scale; i < len(actual); i++ {
			container := actual[i]
			eg.Go(func() error {
				err := s.containerService.Stop(ctx, container.ID, nil)
				if err != nil {
					return err
				}
				return s.containerService.Delete(ctx, container.ID, containers.DeleteRequest{})
			})
		}
		actual = actual[:scale]
	}

	expected, err := jsonHash(service)
	if err != nil {
		return err
	}

	for _, container := range actual {
		container := container
		diverged := container.Labels[configHashLabel] != expected
		if diverged || service.Extensions[extLifecycle] == forceRecreate {
			eg.Go(func() error {
				return s.recreateContainer(ctx, project, service, container)
			})
			continue
		}

		if container.State == "running" {
			// already running, skip
			continue
		}

		eg.Go(func() error {
			return s.restartContainer(ctx, service, container)
		})
	}
	return eg.Wait()
}

func (s *local) waitDependencies(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	eg, ctx := errgroup.WithContext(ctx)
	for dep, config := range service.DependsOn {
		switch config.Condition {
		case "service_healthy":
			eg.Go(func() error {
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				for {
					<-ticker.C
					healthy, err := s.isServiceHealthy(ctx, project, dep)
					if err != nil {
						return err
					}
					if healthy {
						return nil
					}
				}
			})
		}
	}
	return eg.Wait()
}

func nextContainerNumber(containers []moby.Container) (int, error) {
	max := 0
	for _, c := range containers {
		n, err := strconv.Atoi(c.Labels[containerNumberLabel])
		if err != nil {
			return 0, err
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil

}

func getScale(config types.ServiceConfig) int {
	if config.Deploy != nil && config.Deploy.Replicas != nil {
		return int(*config.Deploy.Replicas)
	}
	if config.Scale != 0 {
		return config.Scale
	}
	return 1
}

func (s *local) createContainer(ctx context.Context, project *types.Project, service types.ServiceConfig, name string, number int) error {
	w := progress.ContextWriter(ctx)
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Working,
		StatusText: "Create",
	})
	err := s.runContainer(ctx, project, service, name, number, nil)
	if err != nil {
		return err
	}
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Done,
		StatusText: "Created",
	})
	return nil
}

func (s *local) recreateContainer(ctx context.Context, project *types.Project, service types.ServiceConfig, container moby.Container) error {
	w := progress.ContextWriter(ctx)
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Working,
		StatusText: "Recreate",
	})
	err := s.containerService.Stop(ctx, container.ID, nil)
	if err != nil {
		return err
	}
	name := getContainerName(container)
	tmpName := fmt.Sprintf("%s_%s", container.ID[:12], name)
	err = s.containerService.apiClient.ContainerRename(ctx, container.ID, tmpName)
	if err != nil {
		return err
	}
	number, err := strconv.Atoi(container.Labels[containerNumberLabel])
	if err != nil {
		return err
	}
	err = s.runContainer(ctx, project, service, name, number, &container)
	if err != nil {
		return err
	}
	err = s.containerService.Delete(ctx, container.ID, containers.DeleteRequest{})
	if err != nil {
		return err
	}
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Done,
		StatusText: "Recreated",
	})
	setDependentLifecycle(project, service.Name, forceRecreate)
	return nil
}

// setDependentLifecycle define the Lifecycle strategy for all services to depend on specified service
func setDependentLifecycle(project *types.Project, service string, strategy string) {
	for i, s := range project.Services {
		if contains(s.GetDependencies(), service) {
			if s.Extensions == nil {
				s.Extensions = map[string]interface{}{}
			}
			s.Extensions[extLifecycle] = strategy
			project.Services[i] = s
		}
	}
}

func (s *local) restartContainer(ctx context.Context, service types.ServiceConfig, container moby.Container) error {
	w := progress.ContextWriter(ctx)
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Working,
		StatusText: "Restart",
	})
	err := s.containerService.Start(ctx, container.ID)
	if err != nil {
		return err
	}
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Service %q", service.Name),
		Status:     progress.Done,
		StatusText: "Restarted",
	})
	return nil
}

func (s *local) runContainer(ctx context.Context, project *types.Project, service types.ServiceConfig, name string, number int, container *moby.Container) error {
	containerConfig, hostConfig, networkingConfig, err := getContainerCreateOptions(project, service, number, container)
	if err != nil {
		return err
	}
	id, err := s.containerService.create(ctx, containerConfig, hostConfig, networkingConfig, name)
	if err != nil {
		return err
	}
	for net := range service.Networks {
		name := fmt.Sprintf("%s_%s", project.Name, net)
		err = s.connectContainerToNetwork(ctx, id, service.Name, name)
		if err != nil {
			return err
		}
	}
	err = s.containerService.apiClient.ContainerStart(ctx, id, moby.ContainerStartOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (s *local) connectContainerToNetwork(ctx context.Context, id string, service string, n string) error {
	err := s.containerService.apiClient.NetworkConnect(ctx, n, id, &network.EndpointSettings{
		Aliases: []string{service},
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *local) isServiceHealthy(ctx context.Context, project *types.Project, service string) (bool, error) {
	containers, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("%s=%s", projectLabel, project.Name)),
			filters.Arg("label", fmt.Sprintf("%s=%s", serviceLabel, service)),
		),
	})
	if err != nil {
		return false, err
	}

	for _, c := range containers {
		container, err := s.containerService.apiClient.ContainerInspect(ctx, c.ID)
		if err != nil {
			return false, err
		}
		if container.State == nil || container.State.Health == nil {
			return false, fmt.Errorf("container for service %q has no healthcheck configured", service)
		}
		switch container.State.Health.Status {
		case "starting":
			return false, nil
		case "unhealthy":
			return false, nil
		}
	}
	return true, nil

}
