package service

import (
	"context"
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
)

func (p *Platform) ServiceInit(ctx context.Context, req api.ServiceInitRequest) error {
	if req.Name == "" {
		return &InvalidInputError{Field: "name", Reason: "service name is required"}
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if _, exists := stack.Services[req.Name]; exists {
		return &ConflictError{Kind: "service", Name: req.Name, Reason: "already exists"}
	}
	service, err := serviceFromRequest(req)
	if err != nil {
		return err
	}
	stack.Services[req.Name] = service
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return err
	}
	if _, err := p.StackPrepare(ctx); err != nil {
		return err
	}
	if req.Start {
		return p.ServiceStart(ctx, []string{req.Name})
	}
	return nil
}

func (p *Platform) ServiceUpdate(ctx context.Context, req api.ServiceInitRequest) error {
	if req.Name == "" {
		return &InvalidInputError{Field: "name", Reason: "service name is required"}
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	current, exists := stack.Services[req.Name]
	if !exists {
		return &NotFoundError{Kind: "service", Name: req.Name}
	}
	updated := current
	if req.Runtime != "" {
		updated.Runtime = manifest.Runtime(req.Runtime)
	}
	if req.Image != "" {
		updated.Image = req.Image
	}
	if req.Command != nil {
		updated.Command = req.Command
	}
	if req.Env != nil {
		updated.Env = req.Env
	}
	if req.Mounts != nil {
		updated.Mounts = manifest.StringList(req.Mounts)
	}
	if req.Ports != nil {
		updated.Ports = manifest.StringList(req.Ports)
	}
	if req.Workdir != "" {
		updated.Workdir = req.Workdir
	}
	if err := validateService(req.Name, updated); err != nil {
		return err
	}
	stack.Services[req.Name] = updated
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return err
	}
	_, err = p.StackPrepare(ctx)
	return err
}

func (p *Platform) ServiceDestroy(ctx context.Context, name string, stop bool) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	service, exists := stack.Services[name]
	if !exists {
		return &NotFoundError{Kind: "service", Name: name}
	}
	if stop && service.Runtime == manifest.RuntimeContainer {
		_ = p.ServiceStop(ctx, []string{name})
	}
	delete(stack.Services, name)
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return err
	}
	_, err = p.StackPrepare(ctx)
	return err
}

func (p *Platform) ServiceList(ctx context.Context) ([]api.ServiceState, error) {
	status, err := p.StackStatus(ctx)
	if err != nil {
		return nil, err
	}
	services := make([]api.ServiceState, 0, len(status.Services))
	for _, name := range sortedKeys(status.Services) {
		services = append(services, status.Services[name])
	}
	return services, nil
}

func serviceFromRequest(req api.ServiceInitRequest) (manifest.Service, error) {
	runtimeKind := manifest.Runtime(req.Runtime)
	if runtimeKind == "" {
		switch {
		case req.Image != "":
			runtimeKind = manifest.RuntimeContainer
		case len(req.Command) > 0:
			runtimeKind = manifest.RuntimeLocal
		default:
			return manifest.Service{}, &InvalidInputError{Field: "service", Reason: fmt.Sprintf("%q requires --image or --command", req.Name)}
		}
	}
	service := manifest.Service{
		Runtime: runtimeKind,
		Image:   req.Image,
		Command: req.Command,
		Env:     req.Env,
		Mounts:  manifest.StringList(req.Mounts),
		Ports:   manifest.StringList(req.Ports),
		Workdir: req.Workdir,
	}
	if err := validateService(req.Name, service); err != nil {
		return manifest.Service{}, err
	}
	return service, nil
}

func validateService(name string, service manifest.Service) error {
	stack := &manifest.Stack{
		Version:  manifest.VersionCurrent,
		Kind:     manifest.KindStack,
		Name:     "validation",
		Services: map[string]manifest.Service{name: service},
	}
	stack.Defaults()
	return stack.Validate()
}
