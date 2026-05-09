package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
	gql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"gopkg.in/yaml.v3"
)

type graphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

const (
	maxGraphQLBodyBytes = 1 << 20
	maxGraphQLLogBytes  = 1 << 20
)

var errUnsupportedGraphQLMediaType = errors.New("unsupported GraphQL content type")

func newGraphQLHandler(s *Server) (http.Handler, error) {
	jsonScalar := gql.NewScalar(gql.ScalarConfig{
		Name:        "JSON",
		Description: "Arbitrary JSON data returned by the operator.",
		Serialize: func(value any) any {
			return value
		},
		ParseValue: func(value any) any {
			return value
		},
		ParseLiteral: parseJSONLiteral,
	})

	keyValueType := gql.NewObject(gql.ObjectConfig{
		Name: "KeyValue",
		Fields: gql.Fields{
			"key":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"value": &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	serviceStateType := gql.NewObject(gql.ObjectConfig{
		Name: "ServiceState",
		Fields: gql.Fields{
			"name":    &gql.Field{Type: gql.NewNonNull(gql.String)},
			"runtime": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"status":  &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	jobStateType := gql.NewObject(gql.ObjectConfig{
		Name: "JobState",
		Fields: gql.Fields{
			"name":    &gql.Field{Type: gql.NewNonNull(gql.String)},
			"runtime": &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	workspaceRefType := gql.NewObject(gql.ObjectConfig{
		Name: "WorkspaceRef",
		Fields: gql.Fields{
			"name":     &gql.Field{Type: gql.NewNonNull(gql.String)},
			"path":     &gql.Field{Type: gql.NewNonNull(gql.String)},
			"template": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"ttl":      &gql.Field{Type: gql.String},
			"ttlExpiresAt": &gql.Field{
				Type: gql.String,
				Resolve: func(p gql.ResolveParams) (any, error) {
					ref, ok := p.Source.(api.WorkspaceRef)
					if !ok {
						return nil, nil
					}
					return formatGraphQLTime(ref.TTLExpiresAt), nil
				},
			},
		},
	})

	sourceStateType := gql.NewObject(gql.ObjectConfig{
		Name: "SourceState",
		Fields: gql.Fields{
			"name":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"slot":   &gql.Field{Type: gql.String},
			"kind":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"path":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"exists": &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"ref":    &gql.Field{Type: gql.String},
			"dirty":  &gql.Field{Type: gql.Boolean},
			"error":  &gql.Field{Type: gql.String},
		},
	})

	workspaceSourceStatusType := gql.NewObject(gql.ObjectConfig{
		Name: "WorkspaceSourceStatus",
		Fields: gql.Fields{
			"slot":           &gql.Field{Type: gql.NewNonNull(gql.String)},
			"source":         &gql.Field{Type: gql.NewNonNull(gql.String)},
			"kind":           &gql.Field{Type: gql.NewNonNull(gql.String)},
			"mode":           &gql.Field{Type: gql.String},
			"branch":         &gql.Field{Type: gql.String},
			"ref":            &gql.Field{Type: gql.String},
			"subpath":        &gql.Field{Type: gql.String},
			"path":           &gql.Field{Type: gql.NewNonNull(gql.String)},
			"exists":         &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"state":          &gql.Field{Type: gql.NewNonNull(gql.String)},
			"currentRef":     &gql.Field{Type: gql.String},
			"dirty":          &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"upstream":       &gql.Field{Type: gql.String},
			"ahead":          &gql.Field{Type: gql.Int},
			"behind":         &gql.Field{Type: gql.Int},
			"pushed":         &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"unpushedReason": &gql.Field{Type: gql.String},
			"error":          &gql.Field{Type: gql.String},
		},
	})

	workspaceMountRefType := gql.NewObject(gql.ObjectConfig{
		Name: "WorkspaceMountRef",
		Fields: gql.Fields{
			"kind":  &gql.Field{Type: gql.NewNonNull(gql.String)},
			"name":  &gql.Field{Type: gql.NewNonNull(gql.String)},
			"field": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"value": &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	stackStatusType := gql.NewObject(gql.ObjectConfig{
		Name: "StackStatus",
		Fields: gql.Fields{
			"root": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"name": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"services": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(serviceStateType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					status, ok := p.Source.(api.StackStatusResponse)
					if !ok {
						return nil, nil
					}
					return sortedServiceStates(status.Services), nil
				},
			},
			"jobs": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(jobStateType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					status, ok := p.Source.(api.StackStatusResponse)
					if !ok {
						return nil, nil
					}
					return sortedJobStates(status.Jobs), nil
				},
			},
			"workspaces": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(workspaceRefType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					status, ok := p.Source.(api.StackStatusResponse)
					if !ok {
						return nil, nil
					}
					return sortedWorkspaceRefs(status.Workspaces), nil
				},
			},
		},
	})

	workspaceStatusType := gql.NewObject(gql.ObjectConfig{
		Name: "WorkspaceStatus",
		Fields: gql.Fields{
			"name":        &gql.Field{Type: gql.NewNonNull(gql.String)},
			"path":        &gql.Field{Type: gql.NewNonNull(gql.String)},
			"exists":      &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"state":       &gql.Field{Type: gql.NewNonNull(gql.String)},
			"error":       &gql.Field{Type: gql.String},
			"template":    &gql.Field{Type: gql.NewNonNull(gql.String)},
			"inputs":      &gql.Field{Type: jsonScalar},
			"sources":     &gql.Field{Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(workspaceSourceStatusType)))},
			"chain":       &gql.Field{Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(gql.String)))},
			"chainRoot":   &gql.Field{Type: gql.String},
			"lifecycle":   &gql.Field{Type: gql.String},
			"allocations": &gql.Field{Type: jsonScalar},
			"persistPaths": &gql.Field{
				Type: jsonScalar,
			},
			"ttl": &gql.Field{Type: gql.String},
			"ttlExpiresAt": &gql.Field{
				Type: gql.String,
				Resolve: func(p gql.ResolveParams) (any, error) {
					status, ok := p.Source.(api.WorkspaceStatusResponse)
					if !ok {
						return nil, nil
					}
					return formatGraphQLTime(status.TTLExpiresAt), nil
				},
			},
			"expired":    &gql.Field{Type: gql.NewNonNull(gql.Boolean)},
			"mountedBy":  &gql.Field{Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(workspaceMountRefType)))},
			"innerStack": &gql.Field{Type: stackStatusType},
			"innerError": &gql.Field{Type: gql.String},
		},
	})

	compiledStackType := gql.NewObject(gql.ObjectConfig{
		Name: "CompiledStack",
		Fields: gql.Fields{
			"compose": &gql.Field{
				Type: jsonScalar,
				Resolve: func(p gql.ResolveParams) (any, error) {
					compiled, ok := p.Source.(*service.CompiledStack)
					if !ok || compiled == nil {
						return nil, nil
					}
					return yamlTaggedValue(compiled.Compose)
				},
			},
			"processCompose": &gql.Field{
				Type: jsonScalar,
				Resolve: func(p gql.ResolveParams) (any, error) {
					compiled, ok := p.Source.(*service.CompiledStack)
					if !ok || compiled == nil {
						return nil, nil
					}
					return yamlTaggedValue(compiled.ProcessCompose)
				},
			},
			"secretEnvVars": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(keyValueType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					compiled, ok := p.Source.(*service.CompiledStack)
					if !ok || compiled == nil {
						return nil, nil
					}
					return keyValueList(compiled.SecretEnvVars), nil
				},
			},
		},
	})

	resultType := gql.NewObject(gql.ObjectConfig{
		Name: "MutationResult",
		Fields: gql.Fields{
			"status":  &gql.Field{Type: gql.NewNonNull(gql.String)},
			"name":    &gql.Field{Type: gql.String},
			"message": &gql.Field{Type: gql.String},
		},
	})

	stackInitResultType := gql.NewObject(gql.ObjectConfig{
		Name: "StackInitResult",
		Fields: gql.Fields{
			"status":   &gql.Field{Type: gql.NewNonNull(gql.String)},
			"template": &gql.Field{Type: gql.NewNonNull(gql.String)},
			"root":     &gql.Field{Type: gql.NewNonNull(gql.String)},
		},
	})

	keyValueInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "KeyValueInput",
		Fields: gql.InputObjectConfigFieldMap{
			"key":   &gql.InputObjectFieldConfig{Type: gql.NewNonNull(gql.String)},
			"value": &gql.InputObjectFieldConfig{Type: gql.NewNonNull(gql.String)},
		},
	})

	stackInitInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "StackInitInput",
		Fields: gql.InputObjectConfigFieldMap{
			"template": &gql.InputObjectFieldConfig{Type: gql.NewNonNull(gql.String)},
			"path":     &gql.InputObjectFieldConfig{Type: gql.String},
			"inputs":   &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(keyValueInput))},
			"force":    &gql.InputObjectFieldConfig{Type: gql.Boolean},
		},
	})

	stackRuntimeInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "StackRuntimeInput",
		Fields: gql.InputObjectConfigFieldMap{
			"services": &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(gql.String))},
			"build":    &gql.InputObjectFieldConfig{Type: gql.Boolean},
		},
	})

	serviceInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "ServiceInput",
		Fields: gql.InputObjectConfigFieldMap{
			"name":    &gql.InputObjectFieldConfig{Type: gql.String},
			"runtime": &gql.InputObjectFieldConfig{Type: gql.String},
			"image":   &gql.InputObjectFieldConfig{Type: gql.String},
			"command": &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(gql.String))},
			"mounts":  &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(gql.String))},
			"env":     &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(keyValueInput))},
			"ports":   &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(gql.String))},
			"workdir": &gql.InputObjectFieldConfig{Type: gql.String},
			"start":   &gql.InputObjectFieldConfig{Type: gql.Boolean},
		},
	})

	workspaceCreateInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "WorkspaceCreateInput",
		Fields: gql.InputObjectConfigFieldMap{
			"template": &gql.InputObjectFieldConfig{Type: gql.NewNonNull(gql.String)},
			"name":     &gql.InputObjectFieldConfig{Type: gql.String},
			"inputs":   &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(keyValueInput))},
			"ttl":      &gql.InputObjectFieldConfig{Type: gql.String},
			"start":    &gql.InputObjectFieldConfig{Type: gql.Boolean},
		},
	})

	workspaceUpdateInput := gql.NewInputObject(gql.InputObjectConfig{
		Name: "WorkspaceUpdateInput",
		Fields: gql.InputObjectConfigFieldMap{
			"inputs": &gql.InputObjectFieldConfig{Type: gql.NewList(gql.NewNonNull(keyValueInput))},
			"ttl":    &gql.InputObjectFieldConfig{Type: gql.String},
		},
	})

	queryType := gql.NewObject(gql.ObjectConfig{
		Name: "Query",
		Fields: gql.Fields{
			"health": &gql.Field{
				Type: resultType,
				Resolve: func(gql.ResolveParams) (any, error) {
					return actionResult("ok"), nil
				},
			},
			"stackStatus": &gql.Field{
				Type: stackStatusType,
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.StackStatus(p.Context)
				},
			},
			"services": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(serviceStateType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.ServiceList(p.Context)
				},
			},
			"jobs": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(jobStateType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.JobList(p.Context)
				},
			},
			"sources": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(sourceStateType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.SourceList(p.Context)
				},
			},
			"source": &gql.Field{
				Type: sourceStateType,
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.SourceStatus(p.Context, stringArg(p.Args, "name"))
				},
			},
			"workspaces": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(workspaceRefType))),
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspaceList(p.Context)
				},
			},
			"workspace": &gql.Field{
				Type: workspaceRefType,
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspaceGet(p.Context, stringArg(p.Args, "name"))
				},
			},
			"workspaceStatus": &gql.Field{
				Type: workspaceStatusType,
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspaceStatus(p.Context, stringArg(p.Args, "name"))
				},
			},
			"workspaceGit": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(sourceStateType))),
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspaceGitStatus(p.Context, stringArg(p.Args, "name"))
				},
			},
			"stackLogs": &gql.Field{
				Type: gql.NewNonNull(gql.String),
				Args: gql.FieldConfigArgument{
					"services": &gql.ArgumentConfig{Type: gql.NewList(gql.NewNonNull(gql.String))},
					"limit":    &gql.ArgumentConfig{Type: gql.Int},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					limit := logLimitArg(p.Args)
					logs, err := s.platform.StackLogsLimited(p.Context, stringSliceArg(p.Args, "services"), false, limit)
					if err != nil {
						return nil, err
					}
					return collectLogStream(logs, limit), nil
				},
			},
			"serviceLogs": &gql.Field{
				Type: gql.NewNonNull(gql.String),
				Args: gql.FieldConfigArgument{
					"name":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"limit": &gql.ArgumentConfig{Type: gql.Int},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					limit := logLimitArg(p.Args)
					logs, err := s.platform.StackLogsLimited(p.Context, []string{stringArg(p.Args, "name")}, false, limit)
					if err != nil {
						return nil, err
					}
					return collectLogStream(logs, limit), nil
				},
			},
			"workspaceLogs": &gql.Field{
				Type: gql.NewNonNull(gql.String),
				Args: gql.FieldConfigArgument{
					"name":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"limit": &gql.ArgumentConfig{Type: gql.Int},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					limit := logLimitArg(p.Args)
					logs, err := s.platform.WorkspaceLogsLimited(p.Context, stringArg(p.Args, "name"), false, limit)
					if err != nil {
						return nil, err
					}
					return collectLogStream(logs, limit), nil
				},
			},
			"mcpDescriptor": &gql.Field{
				Type: jsonScalar,
				Resolve: func(gql.ResolveParams) (any, error) {
					return mcpDescriptor(), nil
				},
			},
		},
	})

	mutationType := gql.NewObject(gql.ObjectConfig{
		Name: "Mutation",
		Fields: gql.Fields{
			"stackInit": &gql.Field{
				Type: stackInitResultType,
				Args: gql.FieldConfigArgument{
					"input": &gql.ArgumentConfig{Type: gql.NewNonNull(stackInitInput)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					input := inputArg(p.Args, "input")
					result, err := s.platform.StackInit(
						p.Context,
						stringFrom(input, "template"),
						stringFrom(input, "path"),
						keyValuesFrom(input, "inputs"),
						boolFrom(input, "force"),
					)
					if err != nil {
						return nil, err
					}
					return map[string]any{"status": "initialized", "template": result.Template, "root": result.Root}, nil
				},
			},
			"stackUpdate": &gql.Field{
				Type: resultType,
				Resolve: func(p gql.ResolveParams) (any, error) {
					if err := s.platform.StackUpdate(p.Context); err != nil {
						return nil, err
					}
					return actionResult("updated"), nil
				},
			},
			"stackPrepare": &gql.Field{
				Type: compiledStackType,
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.StackPrepare(p.Context)
				},
			},
			"stackBuild": stackRuntimeMutation(resultType, stackRuntimeInput, "built", func(ctx context.Context, req api.StackRuntimeRequest) error {
				return s.platform.StackBuild(ctx, req.Services)
			}),
			"stackUp": stackRuntimeMutation(resultType, stackRuntimeInput, "started", func(ctx context.Context, req api.StackRuntimeRequest) error {
				return s.platform.StackUp(ctx, req.Services, req.Build)
			}),
			"stackDev": stackRuntimeMutation(resultType, stackRuntimeInput, "started", func(ctx context.Context, req api.StackRuntimeRequest) error {
				return s.platform.StackDev(ctx, req.Build)
			}),
			"stackDown": &gql.Field{
				Type: resultType,
				Resolve: func(p gql.ResolveParams) (any, error) {
					if err := s.platform.StackDown(p.Context); err != nil {
						return nil, err
					}
					return actionResult("stopped"), nil
				},
			},
			"stackDestroy": &gql.Field{
				Type: resultType,
				Args: gql.FieldConfigArgument{
					"purge": &gql.ArgumentConfig{Type: gql.Boolean},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					if err := s.platform.StackDestroy(p.Context, boolArg(p.Args, "purge")); err != nil {
						return nil, err
					}
					return actionResult("destroyed"), nil
				},
			},
			"jobRun": &gql.Field{
				Type: gql.NewNonNull(gql.String),
				Args: gql.FieldConfigArgument{
					"name":   &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"inputs": &gql.ArgumentConfig{Type: gql.NewList(gql.NewNonNull(keyValueInput))},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					out, err := s.platform.JobRun(p.Context, stringArg(p.Args, "name"), keyValuesArg(p.Args, "inputs"))
					if err != nil {
						return nil, err
					}
					return string(out), nil
				},
			},
			"serviceInit": &gql.Field{
				Type: resultType,
				Args: gql.FieldConfigArgument{
					"input": &gql.ArgumentConfig{Type: gql.NewNonNull(serviceInput)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					req := serviceRequestFrom(inputArg(p.Args, "input"))
					if err := s.platform.ServiceInit(p.Context, req); err != nil {
						return nil, err
					}
					return namedActionResult("created", req.Name), nil
				},
			},
			"serviceUpdate": &gql.Field{
				Type: resultType,
				Args: gql.FieldConfigArgument{
					"name":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"input": &gql.ArgumentConfig{Type: gql.NewNonNull(serviceInput)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					req := serviceRequestFrom(inputArg(p.Args, "input"))
					req.Name = stringArg(p.Args, "name")
					if err := s.platform.ServiceUpdate(p.Context, req); err != nil {
						return nil, err
					}
					return namedActionResult("updated", req.Name), nil
				},
			},
			"serviceStart":   serviceActionMutation(resultType, "start", "started", s.platform.ServiceStart),
			"serviceStop":    serviceActionMutation(resultType, "stop", "stopped", s.platform.ServiceStop),
			"serviceRestart": serviceActionMutation(resultType, "restart", "restarted", s.platform.ServiceRestart),
			"serviceDestroy": &gql.Field{
				Type: resultType,
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					name := stringArg(p.Args, "name")
					if err := s.platform.ServiceDestroy(p.Context, name, true); err != nil {
						return nil, err
					}
					return namedActionResult("destroyed", name), nil
				},
			},
			"sourceFetch": sourceMutation(sourceStateType, func(p gql.ResolveParams, name string) (api.SourceState, error) {
				return s.platform.SourceFetch(p.Context, name)
			}),
			"sourcePull": sourceMutation(sourceStateType, func(p gql.ResolveParams, name string) (api.SourceState, error) {
				return s.platform.SourcePull(p.Context, name)
			}),
			"sourcePush": &gql.Field{
				Type: sourceStateType,
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"ref":  &gql.ArgumentConfig{Type: gql.String},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.SourcePush(p.Context, stringArg(p.Args, "name"), stringArg(p.Args, "ref"))
				},
			},
			"workspaceCreate": &gql.Field{
				Type: workspaceRefType,
				Args: gql.FieldConfigArgument{
					"input": &gql.ArgumentConfig{Type: gql.NewNonNull(workspaceCreateInput)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspaceCreate(p.Context, workspaceCreateRequestFrom(inputArg(p.Args, "input")))
				},
			},
			"workspaceUpdate": &gql.Field{
				Type: workspaceRefType,
				Args: gql.FieldConfigArgument{
					"name":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"input": &gql.ArgumentConfig{Type: gql.NewNonNull(workspaceUpdateInput)},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					input := inputArg(p.Args, "input")
					return s.platform.WorkspaceUpdate(p.Context, stringArg(p.Args, "name"), keyValuesFrom(input, "inputs"), stringFrom(input, "ttl"))
				},
			},
			"workspaceStart":   workspaceLifecycleMutation(resultType, "started", s.platform.WorkspaceStart),
			"workspaceStop":    workspaceLifecycleMutation(resultType, "stopped", s.platform.WorkspaceStop),
			"workspaceRestart": workspaceRestartMutation(resultType, s),
			"workspaceDestroy": &gql.Field{
				Type: resultType,
				Args: gql.FieldConfigArgument{
					"name":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"purge": &gql.ArgumentConfig{Type: gql.Boolean},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					name := stringArg(p.Args, "name")
					if err := s.platform.WorkspaceDestroy(p.Context, name, boolArg(p.Args, "purge")); err != nil {
						return nil, err
					}
					return namedActionResult("destroyed", name), nil
				},
			},
			"workspacePush": &gql.Field{
				Type: gql.NewNonNull(gql.NewList(gql.NewNonNull(sourceStateType))),
				Args: gql.FieldConfigArgument{
					"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
					"ref":  &gql.ArgumentConfig{Type: gql.String},
				},
				Resolve: func(p gql.ResolveParams) (any, error) {
					return s.platform.WorkspacePush(p.Context, stringArg(p.Args, "name"), stringArg(p.Args, "ref"))
				},
			},
		},
	})

	schema, err := gql.NewSchema(gql.SchemaConfig{Query: queryType, Mutation: mutationType})
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxGraphQLBodyBytes)
		req, err := decodeGraphQLRequest(r)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				return
			}
			if errors.Is(err, errUnsupportedGraphQLMediaType) {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": err.Error()})
				return
			}
			writeBadRequest(w, err)
			return
		}
		result := gql.Do(gql.Params{
			Schema:         schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        r.Context(),
		})
		status := http.StatusOK
		if len(result.Errors) > 0 && result.Data == nil {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, result)
	}), nil
}

func decodeGraphQLRequest(r *http.Request) (graphQLRequest, error) {
	switch r.Method {
	case http.MethodPost:
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			return graphQLRequest{}, errUnsupportedGraphQLMediaType
		}
		switch mediaType {
		case "application/graphql":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				return graphQLRequest{}, err
			}
			return validateGraphQLRequest(graphQLRequest{Query: string(data)})
		case "application/json":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				return graphQLRequest{}, err
			}
			var req graphQLRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return graphQLRequest{}, err
			}
			return validateGraphQLRequest(req)
		default:
			return graphQLRequest{}, errUnsupportedGraphQLMediaType
		}
	default:
		return graphQLRequest{}, fmt.Errorf("unsupported GraphQL method %s", r.Method)
	}
}

func validateGraphQLRequest(req graphQLRequest) (graphQLRequest, error) {
	if strings.TrimSpace(req.Query) == "" {
		return graphQLRequest{}, fmt.Errorf("graphql query is required")
	}
	return req, nil
}

func parseJSONLiteral(valueAST ast.Value) any {
	switch value := valueAST.(type) {
	case *ast.StringValue:
		return value.Value
	case *ast.BooleanValue:
		return value.Value
	case *ast.IntValue:
		parsed, err := strconv.ParseInt(value.Value, 10, 64)
		if err != nil {
			return nil
		}
		return parsed
	case *ast.FloatValue:
		parsed, err := strconv.ParseFloat(value.Value, 64)
		if err != nil {
			return nil
		}
		return parsed
	case *ast.ListValue:
		items := make([]any, 0, len(value.Values))
		for _, item := range value.Values {
			items = append(items, parseJSONLiteral(item))
		}
		return items
	case *ast.ObjectValue:
		out := map[string]any{}
		for _, field := range value.Fields {
			out[field.Name.Value] = parseJSONLiteral(field.Value)
		}
		return out
	default:
		return nil
	}
}

func stackRuntimeMutation(resultType *gql.Object, inputType *gql.InputObject, status string, run func(context.Context, api.StackRuntimeRequest) error) *gql.Field {
	return &gql.Field{
		Type: resultType,
		Args: gql.FieldConfigArgument{
			"input": &gql.ArgumentConfig{Type: inputType},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			req := stackRuntimeRequestFrom(inputArg(p.Args, "input"))
			if err := run(p.Context, req); err != nil {
				return nil, err
			}
			return actionResult(status), nil
		},
	}
}

func serviceActionMutation(resultType *gql.Object, action, status string, run func(context.Context, []string) error) *gql.Field {
	return &gql.Field{
		Type: resultType,
		Args: gql.FieldConfigArgument{
			"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			name := stringArg(p.Args, "name")
			if err := run(p.Context, []string{name}); err != nil {
				return nil, err
			}
			return namedActionResult(status, name), nil
		},
		Description: action,
	}
}

func sourceMutation(sourceStateType *gql.Object, run func(gql.ResolveParams, string) (api.SourceState, error)) *gql.Field {
	return &gql.Field{
		Type: sourceStateType,
		Args: gql.FieldConfigArgument{
			"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			return run(p, stringArg(p.Args, "name"))
		},
	}
}

func workspaceLifecycleMutation(resultType *gql.Object, status string, run func(context.Context, string) error) *gql.Field {
	return &gql.Field{
		Type: resultType,
		Args: gql.FieldConfigArgument{
			"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			name := stringArg(p.Args, "name")
			if err := run(p.Context, name); err != nil {
				return nil, err
			}
			return namedActionResult(status, name), nil
		},
	}
}

func workspaceRestartMutation(resultType *gql.Object, s *Server) *gql.Field {
	return &gql.Field{
		Type: resultType,
		Args: gql.FieldConfigArgument{
			"name": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			name := stringArg(p.Args, "name")
			if err := s.platform.WorkspaceStop(p.Context, name); err != nil {
				return nil, err
			}
			if err := s.platform.WorkspaceStart(p.Context, name); err != nil {
				return nil, err
			}
			return namedActionResult("restarted", name), nil
		},
	}
}

func actionResult(status string) map[string]any {
	return map[string]any{"status": status}
}

func namedActionResult(status, name string) map[string]any {
	return map[string]any{"status": status, "name": name}
}

func serviceRequestFrom(input map[string]any) api.ServiceInitRequest {
	return api.ServiceInitRequest{
		Name:    stringFrom(input, "name"),
		Runtime: stringFrom(input, "runtime"),
		Image:   stringFrom(input, "image"),
		Command: stringSliceFrom(input, "command"),
		Mounts:  stringSliceFrom(input, "mounts"),
		Env:     keyValuesFrom(input, "env"),
		Ports:   stringSliceFrom(input, "ports"),
		Workdir: stringFrom(input, "workdir"),
		Start:   boolFrom(input, "start"),
	}
}

func workspaceCreateRequestFrom(input map[string]any) api.WorkspaceCreateRequest {
	return api.WorkspaceCreateRequest{
		Template: stringFrom(input, "template"),
		Name:     stringFrom(input, "name"),
		Inputs:   keyValuesFrom(input, "inputs"),
		TTL:      stringFrom(input, "ttl"),
		Start:    boolFrom(input, "start"),
	}
}

func stackRuntimeRequestFrom(input map[string]any) api.StackRuntimeRequest {
	return api.StackRuntimeRequest{
		Services: stringSliceFrom(input, "services"),
		Build:    boolFrom(input, "build"),
	}
}

func inputArg(args map[string]any, key string) map[string]any {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	if input, ok := raw.(map[string]any); ok {
		return input
	}
	return nil
}

func stringArg(args map[string]any, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	if value, ok := args[key].(bool); ok {
		return value
	}
	return false
}

func intArg(args map[string]any, key string) int {
	switch value := args[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func logLimitArg(args map[string]any) int {
	limit := intArg(args, "limit")
	if limit <= 0 || limit > maxGraphQLLogBytes {
		return maxGraphQLLogBytes
	}
	return limit
}

func stringSliceArg(args map[string]any, key string) []string {
	return stringSliceValue(args[key])
}

func keyValuesArg(args map[string]any, key string) map[string]string {
	return keyValuesValue(args[key])
}

func stringFrom(input map[string]any, key string) string {
	if input == nil {
		return ""
	}
	if value, ok := input[key].(string); ok {
		return value
	}
	return ""
}

func boolFrom(input map[string]any, key string) bool {
	if input == nil {
		return false
	}
	if value, ok := input[key].(bool); ok {
		return value
	}
	return false
}

func stringSliceFrom(input map[string]any, key string) []string {
	if input == nil {
		return nil
	}
	return stringSliceValue(input[key])
}

func keyValuesFrom(input map[string]any, key string) map[string]string {
	if input == nil {
		return nil
	}
	return keyValuesValue(input[key])
}

func stringSliceValue(raw any) []string {
	if raw == nil {
		return nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func keyValuesValue(raw any) map[string]string {
	if raw == nil {
		return nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		key, _ := entry["key"].(string)
		val, _ := entry["value"].(string)
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func collectLogStream(logs <-chan string, limit int) string {
	var out strings.Builder
	remaining := limit
	truncated := false
	for line := range logs {
		if remaining <= 0 {
			if !truncated {
				out.WriteString("\n[truncated]\n")
				truncated = true
			}
			continue
		}
		if len(line) > remaining {
			out.WriteString(line[:remaining])
			out.WriteString("\n[truncated]\n")
			remaining = 0
			truncated = true
			continue
		}
		out.WriteString(line)
		remaining -= len(line)
	}
	return out.String()
}

func yamlTaggedValue(value any) (any, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return normalizeYAMLValue(decoded), nil
}

func normalizeYAMLValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range value {
			out[key] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for key, item := range value {
			out[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return value
	}
}

func keyValueList(values map[string]string) []map[string]any {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{"key": key, "value": values[key]})
	}
	return out
}

func sortedServiceStates(values map[string]api.ServiceState) []api.ServiceState {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]api.ServiceState, 0, len(keys))
	for _, key := range keys {
		out = append(out, values[key])
	}
	return out
}

func sortedJobStates(values map[string]api.JobState) []api.JobState {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]api.JobState, 0, len(keys))
	for _, key := range keys {
		out = append(out, values[key])
	}
	return out
}

func sortedWorkspaceRefs(values map[string]api.WorkspaceRef) []api.WorkspaceRef {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]api.WorkspaceRef, 0, len(keys))
	for _, key := range keys {
		out = append(out, values[key])
	}
	return out
}

func formatGraphQLTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func mcpDescriptor() map[string]any {
	return map[string]any{
		"name": "angee-operator",
		"tools": []string{
			"stack.status",
			"stack.up",
			"stack.down",
			"services.create",
			"workspaces.create",
			"sources.fetch",
		},
	}
}
