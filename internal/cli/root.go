package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator"
	"github.com/ang-ee/angee-operator/internal/platformclient"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/ang-ee/angee-operator/internal/stackroot"
	"github.com/spf13/cobra"
)

var Version = "dev"

func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return NewRootWithIO(os.Stdin, os.Stdout, os.Stderr).ExecuteContext(ctx)
}

func NewRoot(stdout, stderr io.Writer) *cobra.Command {
	return NewRootWithIO(strings.NewReader(""), stdout, stderr)
}

func NewRootWithIO(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	var root string
	var operatorURL string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:           "angee",
		Short:         "Stack manager for angee.ai",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.PersistentFlags().StringVar(&root, "root", ".", "ANGEE_ROOT containing angee.yaml")
	cmd.PersistentFlags().StringVar(&operatorURL, "operator", os.Getenv("ANGEE_OPERATOR_URL"), "operator URL for HTTP mode")
	cmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "write JSON output")

	cmd.AddCommand(initCommand(stdout, stderr, &root, &operatorURL))
	cmd.AddCommand(stackCommand(stdout, &root, &operatorURL))
	cmd.AddCommand(statusCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(runtimeCommands(stdout, &root, &operatorURL)...)
	cmd.AddCommand(serviceCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(jobCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(sourceCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(workspaceCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(gitopsCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(templateCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(tokenCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(secretCommand(stdout, stderr, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(fileCommand(stdout, stderr, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(doctorCommand(stdout, &root, &jsonOutput))
	cmd.AddCommand(internalCommand(stdout, &root, &operatorURL, &jsonOutput))
	cmd.AddCommand(operatorCommand(stdout, stderr))
	return cmd
}

func initCommand(stdout, stderr io.Writer, root, operatorURL *string) *cobra.Command {
	var dev bool
	var force bool
	var yes bool
	var inputs []string
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a stack",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			template := "dev"
			if !dev {
				return fmt.Errorf("init requires --dev or use stack init <template>")
			}
			path := ""
			if len(args) == 1 {
				path = args[0]
			}
			parsedInputs, err := parseKeyValues(inputs)
			if err != nil {
				return err
			}
			platform, err := initPlatform(cmd.Context(), root, operatorURL, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			parsedInputs, err = resolveStackTemplateInputs(cmd, platform, template, parsedInputs, yes)
			if err != nil {
				return err
			}
			result, err := platform.StackInit(cmd.Context(), template, path, parsedInputs, force)
			if err != nil {
				return stackInitError(template, err)
			}
			_, err = fmt.Fprintf(stdout, "stack template %s initialized as %s\n", result.Template, displayPath(result.Root))
			return err
		},
	}
	cmd.Flags().BoolVar(&dev, "dev", false, "use the dev stack template")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a non-empty stack root")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept template defaults and run non-interactively")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "template input K=V")
	cmd.AddCommand(initStackCommand(stdout, root, operatorURL))
	return cmd
}

func initStackCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var template string
	var force bool
	var yes bool
	var inputValues []string
	cmd := &cobra.Command{
		Use:   "stack --template <template> <ANGEE_ROOT>",
		Short: "Initialize a stack root from a template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if template == "" {
				return fmt.Errorf("--template is required")
			}
			inputs, err := parseKeyValues(inputValues)
			if err != nil {
				return err
			}
			if inputs == nil {
				inputs = map[string]string{}
			}
			if _, ok := inputs["ANGEE_ROOT"]; !ok {
				inputs["ANGEE_ROOT"] = "."
			}
			platform, err := initPlatform(cmd.Context(), root, operatorURL, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			inputs, err = resolveStackTemplateInputs(cmd, platform, template, inputs, yes)
			if err != nil {
				return err
			}
			result, err := platform.StackInit(cmd.Context(), template, args[0], inputs, force)
			if err != nil {
				return stackInitError(template, err)
			}
			_, err = fmt.Fprintf(stdout, "stack template %s initialized as %s\n", result.Template, displayPath(result.Root))
			return err
		},
	}
	cmd.Flags().StringVarP(&template, "template", "t", "", "template ref, URL, or path")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a non-empty stack root")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept template defaults and run non-interactively")
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "template input K=V")
	return cmd
}

func stackCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	cmd := &cobra.Command{Use: "stack", Short: "Manage stack configuration"}
	var initInputs []string
	var initForce bool
	var initYes bool
	initCmd := &cobra.Command{
		Use:   "init <template> [path]",
		Short: "Initialize a stack from a template",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 2 {
				path = args[1]
			}
			inputs, err := parseKeyValues(initInputs)
			if err != nil {
				return err
			}
			platform, err := initPlatform(cmd.Context(), root, operatorURL, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			inputs, err = resolveStackTemplateInputs(cmd, platform, args[0], inputs, initYes)
			if err != nil {
				return err
			}
			result, err := platform.StackInit(cmd.Context(), args[0], path, inputs, initForce)
			if err != nil {
				return stackInitError(args[0], err)
			}
			_, err = fmt.Fprintf(stdout, "stack template %s initialized as %s\n", result.Template, displayPath(result.Root))
			return err
		},
	}
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite a non-empty stack root")
	initCmd.Flags().BoolVarP(&initYes, "yes", "y", false, "accept template defaults and run non-interactively")
	initCmd.Flags().StringArrayVar(&initInputs, "input", nil, "template input K=V")
	cmd.AddCommand(initCmd)
	var updateTemplate, updateDryRun, updateOverwrite bool
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Update generated runtime files (with --template, re-render angee.yaml from its stack template first)",
		Long: "Regenerate the derived runtime files from angee.yaml.\n\n" +
			"With --template, first re-render angee.yaml from the stack's Copier template: " +
			"template-origin sections are refreshed (template wins for keys it emits; user-added " +
			"keys and operator/workspaces/port_leases/allocated port values are preserved), then " +
			"the runtime files are regenerated. --template runs locally only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if updateDryRun && !updateTemplate {
				return fmt.Errorf("--dry-run only applies with --template")
			}
			if updateOverwrite && !updateTemplate {
				return fmt.Errorf("--overwrite only applies with --template")
			}
			if updateTemplate {
				if operatorURL != nil && *operatorURL != "" {
					return fmt.Errorf("--template re-renders the local stack template and is not supported with --operator")
				}
				controlRoot, err := stackroot.Resolve(*root)
				if err != nil {
					return err
				}
				platform, err := service.New(controlRoot)
				if err != nil {
					return err
				}
				res, err := platform.StackUpdateFromTemplate(cmd.Context(), service.StackUpdateTemplateOptions{DryRun: updateDryRun, Overwrite: updateOverwrite})
				if err != nil {
					return err
				}
				if !res.Changed {
					_, err = fmt.Fprintln(stdout, "stack template up to date")
					return err
				}
				if len(res.Changes) > 0 {
					for _, change := range res.Changes {
						if _, err := fmt.Fprintln(stdout, "  "+change); err != nil {
							return err
						}
					}
				} else {
					if _, err := fmt.Fprintln(stdout, "  (template-origin sections refreshed)"); err != nil {
						return err
					}
				}
				if updateDryRun {
					_, err = fmt.Fprintln(stdout, "dry run: no template output written")
					return err
				}
				_, err = fmt.Fprintln(stdout, "stack updated from template")
				return err
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.StackUpdate(cmd.Context()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "stack updated")
			return err
		},
	}
	updateCmd.Flags().BoolVar(&updateTemplate, "template", false, "re-render all stack template output before regenerating runtime files")
	updateCmd.Flags().BoolVar(&updateDryRun, "dry-run", false, "with --template, print changes without writing")
	updateCmd.Flags().BoolVar(&updateOverwrite, "overwrite", false, "with --template, replace conflicting locally modified files")
	cmd.AddCommand(updateCmd)
	var purge bool
	destroyCmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy stack runtime resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.StackDestroy(cmd.Context(), purge); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "stack destroyed")
			return err
		},
	}
	destroyCmd.Flags().BoolVar(&purge, "purge", false, "remove runtime state directories")
	cmd.AddCommand(destroyCmd)
	return cmd
}

func runtimeCommands(stdout io.Writer, root, operatorURL *string) []*cobra.Command {
	var build bool
	upCmd := &cobra.Command{
		Use:   "up [service...]",
		Short: "Start container services",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.StackUpForeground(cmd.Context(), args, build, stdout, cmd.ErrOrStderr()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "container services started")
			return err
		},
	}
	upCmd.Flags().BoolVar(&build, "build", false, "build images before starting")

	buildCmd := &cobra.Command{
		Use:   "build [service...]",
		Short: "Build container service images",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.StackBuild(cmd.Context(), args); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "container images built")
			return err
		},
	}

	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Stop runtime backends",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.StackDown(cmd.Context()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "stack stopped")
			return err
		},
	}

	startCmd := serviceActionCommand(stdout, root, operatorURL, "start")
	stopCmd := serviceActionCommand(stdout, root, operatorURL, "stop")
	restartCmd := serviceActionCommand(stdout, root, operatorURL, "restart")

	var follow bool
	logsCmd := &cobra.Command{
		Use:   "logs [service...]",
		Short: "Show service logs",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			lines, err := platform.StackLogs(cmd.Context(), args, follow)
			if err != nil {
				return err
			}
			for line := range lines {
				if _, err := fmt.Fprint(stdout, line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow logs")

	var devBuild bool
	var devDetach bool
	devCmd := &cobra.Command{
		Use:   "dev",
		Short: "Run the local development stack",
		Long: "Run the local development stack, streaming logs from every service\n" +
			"regardless of runtime (container and local) into one foreground stream.\n\n" +
			"Examples:\n" +
			"  angee dev            # bring everything up and stream all logs\n" +
			"  angee dev --build    # rebuild container images first, then stream\n" +
			"  angee dev -d         # start the stack in the background and return",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if devDetach {
				if err := platform.StackDev(cmd.Context(), devBuild); err != nil {
					return err
				}
				_, err := fmt.Fprintln(stdout, "dev stack started in background")
				return err
			}
			return platform.StackDevForeground(cmd.Context(), devBuild, stdout, cmd.ErrOrStderr())
		},
	}
	devCmd.Flags().BoolVar(&devBuild, "build", false, "build container images before starting")
	devCmd.Flags().BoolVarP(&devDetach, "detach", "d", false, "start the stack in the background and return")

	return []*cobra.Command{buildCmd, upCmd, devCmd, downCmd, startCmd, stopCmd, restartCmd, logsCmd}
}

func serviceActionCommand(stdout io.Writer, root, operatorURL *string, action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <service>...",
		Short: action + " services",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			switch action {
			case "up":
				err = platform.ServiceUp(cmd.Context(), args)
			case "start":
				err = platform.ServiceStart(cmd.Context(), args)
			case "stop":
				err = platform.ServiceStop(cmd.Context(), args)
			case "restart":
				err = platform.ServiceRestart(cmd.Context(), args)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "services %s\n", actionPast(action))
			return err
		},
	}
}

func actionPast(action string) string {
	switch action {
	case "up":
		return "brought up"
	case "start":
		return "started"
	case "stop":
		return "stopped"
	case "restart":
		return "restarted"
	default:
		return action
	}
}

func serviceCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Manage services"}
	cmd.AddCommand(serviceInitCommand(stdout, root, operatorURL))
	cmd.AddCommand(serviceCreateCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(serviceUpdateCommand(stdout, root, operatorURL))
	cmd.AddCommand(serviceDestroyCommand(stdout, root, operatorURL))
	cmd.AddCommand(serviceListCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(serviceActionCommand(stdout, root, operatorURL, "up"))
	cmd.AddCommand(serviceActionCommand(stdout, root, operatorURL, "start"))
	cmd.AddCommand(serviceActionCommand(stdout, root, operatorURL, "stop"))
	cmd.AddCommand(serviceActionCommand(stdout, root, operatorURL, "restart"))
	cmd.AddCommand(serviceLogsCommand(stdout, root, operatorURL))
	return cmd
}

func serviceLogsCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show service logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			logs, err := platform.StackLogs(cmd.Context(), args, follow)
			if err != nil {
				return err
			}
			for line := range logs {
				if _, err := fmt.Fprint(stdout, line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow logs")
	return cmd
}

func jobCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "job", Short: "Manage jobs"}
	cmd.AddCommand(jobListCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(jobRunCommand(stdout, root, operatorURL))
	cmd.AddCommand(&cobra.Command{
		Use:   "logs <name>",
		Short: "Show job logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("job logs are returned by job run")
		},
	})
	return cmd
}

func jobListCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List jobs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			jobs, _, err := platform.JobList(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, jobs)
			}
			for _, job := range jobs {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\n", job.Name, job.Runtime); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func jobRunCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var inputValues []string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Run a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := parseKeyValues(inputValues)
			if err != nil {
				return err
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			out, err := platform.JobRun(cmd.Context(), args[0], inputs)
			if len(out) > 0 {
				if _, writeErr := stdout.Write(out); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "job input K=V")
	return cmd
}

func serviceInitCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var req api.ServiceInitRequest
	var env []string
	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Add a service to angee.yaml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			parsedEnv, err := parseKeyValues(env)
			if err != nil {
				return err
			}
			req.Env = parsedEnv
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.ServiceInit(cmd.Context(), req); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "service %s added\n", req.Name)
			return err
		},
	}
	bindServiceFlags(cmd, &req, &env)
	cmd.Flags().BoolVar(&req.Start, "start", false, "start service after adding it")
	return cmd
}

func serviceUpdateCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var req api.ServiceInitRequest
	var env []string
	var fromTemplate, dryRun, overwrite bool
	var inputValues []string
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a service in angee.yaml or re-render its template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			if !fromTemplate && (dryRun || overwrite || len(inputValues) != 0) {
				return fmt.Errorf("--input, --dry-run, and --overwrite only apply with --template")
			}
			if fromTemplate {
				for _, flag := range []string{"runtime", "image", "command", "mount", "env", "port", "workdir"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--%s cannot be combined with --template", flag)
					}
				}
				inputs, err := parseKeyValues(inputValues)
				if err != nil {
					return err
				}
				platform, err := localPlatform(root, operatorURL)
				if err != nil {
					return err
				}
				result, err := platform.ServiceUpdateFromTemplate(cmd.Context(), req.Name, api.ServiceUpdateTemplateRequest{Inputs: inputs, DryRun: dryRun, Overwrite: overwrite})
				if err != nil {
					return err
				}
				for _, change := range result.Changes {
					if _, err := fmt.Fprintf(stdout, "  %s %s\n", change.Kind, change.Path); err != nil {
						return err
					}
				}
				if !result.Changed {
					_, err = fmt.Fprintf(stdout, "service %s template up to date\n", req.Name)
					return err
				}
				if dryRun {
					_, err = fmt.Fprintf(stdout, "dry run: service %s template output not written\n", req.Name)
					return err
				}
				_, err = fmt.Fprintf(stdout, "service %s updated from template\n", req.Name)
				return err
			}
			if len(env) > 0 {
				parsedEnv, err := parseKeyValues(env)
				if err != nil {
					return err
				}
				req.Env = parsedEnv
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.ServiceUpdate(cmd.Context(), req); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "service %s updated\n", req.Name)
			return err
		},
	}
	bindServiceFlags(cmd, &req, &env)
	cmd.Flags().BoolVar(&fromTemplate, "template", false, "re-render the service from its recorded Copier template")
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "with --template, override template input K=V")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "with --template, report changes without writing")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "with --template, replace conflicting local changes")
	return cmd
}

func serviceDestroyCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var stop bool
	cmd := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Remove a service from angee.yaml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.ServiceDestroy(cmd.Context(), args[0], stop); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "service %s removed\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&stop, "stop", true, "stop the service before removing it")
	return cmd
}

func serviceListCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List services",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			services, _, err := platform.ServiceList(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, services)
			}
			for _, service := range services {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", service.Name, service.Runtime, service.Status); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func bindServiceFlags(cmd *cobra.Command, req *api.ServiceInitRequest, env *[]string) {
	cmd.Flags().StringVar(&req.Runtime, "runtime", "", "service runtime: container or local")
	cmd.Flags().StringVar(&req.Image, "image", "", "container image")
	cmd.Flags().StringArrayVar(&req.Command, "command", nil, "command argument, repeat for each arg")
	cmd.Flags().StringArrayVar(&req.Mounts, "mount", nil, "mount URI")
	cmd.Flags().StringArrayVar(env, "env", nil, "environment variable K=V")
	cmd.Flags().StringArrayVar(&req.Ports, "port", nil, "port mapping")
	cmd.Flags().StringVar(&req.Workdir, "workdir", "", "working directory URI or path")
}

func parseKeyValues(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("expected K=V, got %q", value)
		}
		out[key] = val
	}
	return out, nil
}

func stackInitError(template string, err error) error {
	var conflict *service.ConflictError
	if errors.As(err, &conflict) && conflict.Kind == "stack-root" {
		return fmt.Errorf("stack template %s already exists as %s; use --force to overwrite or `angee stack update` to update", template, displayPath(conflict.Name))
	}
	if remote, ok := platformclient.AsConflict(err, "stack-root"); ok {
		return fmt.Errorf("stack template %s already exists as %s; use --force to overwrite or `angee stack update` to update", template, displayPath(remote.Body.Name))
	}
	return err
}

func resolveStackTemplateInputs(cmd *cobra.Command, platform service.API, template string, provided map[string]string, yes bool) (map[string]string, error) {
	if provided == nil {
		provided = map[string]string{}
	}
	if yes {
		return provided, nil
	}
	// Filesystem-path templates (absolute, or containing ".." segments) exist
	// only locally and are rejected by Template()'s introspection guard, which
	// is reachable over REST/GraphQL. StackInit resolves such paths directly,
	// so skip descriptor-derived prompting for them and rely on --input/--yes
	// for any non-default inputs.
	if filepath.IsAbs(template) || strings.Contains(template, "..") {
		return provided, nil
	}
	// Derive the interactive questions from the template descriptor, which is
	// served identically over local, REST, and GraphQL transports — so this
	// works against `--operator` too. desc.Inputs is already sorted by name.
	//
	// These callers always init a STACK template. Template() infers the kind
	// from the ref's first path segment, so a bare name like "dev" needs the
	// "stacks/" family prefix to resolve as a stack template (mirroring
	// resolveTemplate's kind+"s" family). Already-qualified or remote refs
	// contain "/" and pass through unchanged.
	ref := template
	if !strings.Contains(ref, "/") {
		ref = "stacks/" + ref
	}
	desc, err := platform.Template(cmd.Context(), ref)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for key, value := range provided {
		out[key] = value
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	for _, input := range desc.Inputs {
		if !input.Question || input.Generated {
			continue
		}
		key := input.Name
		if _, ok := out[key]; ok {
			continue
		}
		defaultValue := input.Default
		hasDefault := defaultValue != ""
		prompt := key + ": "
		if hasDefault {
			prompt = fmt.Sprintf("%s [%s]: ", key, defaultValue)
		}
		if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
			return nil, err
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return nil, fmt.Errorf("template input %s requires interactive input; use --yes to accept defaults or --input %s=value", key, key)
		}
		value := strings.TrimSpace(line)
		if value == "" && hasDefault {
			value = defaultValue
		}
		if value == "" && input.Required {
			return nil, fmt.Errorf("template input %s is required; pass --input %s=value", key, key)
		}
		if value != "" {
			if err := validateTemplateInputValue(key, input.Type, value); err != nil {
				return nil, err
			}
			out[key] = value
		}
	}
	return out, nil
}

func validateTemplateInputValue(key string, typ string, value string) error {
	switch typ {
	case "", "str", "string", "path":
		return nil
	case "int", "integer":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("template input %s must be an integer", key)
		}
		return nil
	case "bool", "boolean":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("template input %s must be a boolean", key)
		}
		return nil
	default:
		return nil
	}
}

func displayPath(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		return path
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		if rel == "." {
			return rel
		}
		return path
	}
	return rel
}

func localPlatform(root, operatorURL *string) (service.API, error) {
	return localPlatformForRoot(root, operatorURL, true)
}

func localPlatformForRoot(root, operatorURL *string, resolveControlRoot bool) (service.API, error) {
	if operatorURL != nil && *operatorURL != "" {
		return platformclient.New(*operatorURL), nil
	}
	selected := *root
	if resolveControlRoot {
		resolved, err := stackroot.Resolve(selected)
		if err != nil {
			return nil, err
		}
		selected = resolved
	}
	return service.New(selected)
}

// initPlatform selects the platform for stack-init commands. When an operator
// URL is configured but the operator is not reachable, init falls back to an
// in-process local platform instead of failing: init is a bootstrap command
// that only renders template files into a new root, so running it locally is a
// safe default (and often the only option, since the operator for that root may
// not exist yet). A reachable operator, and the no-operator case, behave
// exactly like localPlatformForRoot with resolveControlRoot=false.
func initPlatform(ctx context.Context, root, operatorURL *string, stderr io.Writer) (service.API, error) {
	if operatorURL == nil || *operatorURL == "" {
		return localPlatformForRoot(root, operatorURL, false)
	}
	client := platformclient.New(*operatorURL)
	if err := client.Ping(ctx); err != nil {
		if ctx.Err() != nil {
			// The caller's context was canceled (Ctrl-C / SIGTERM), not the
			// operator refusing the probe. Surface the interruption instead of
			// silently creating a local stack the user tried to abort.
			return nil, err
		}
		_, _ = fmt.Fprintf(stderr, "angee: operator at %s is not reachable (%v); running init locally\n", *operatorURL, err)
		return service.New(*root)
	}
	return client, nil
}

func sourceCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Manage sources"}
	cmd.AddCommand(sourceDiffCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sources",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			sources, _, err := platform.SourceList(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, sources)
			}
			for _, source := range sources {
				exists := "missing"
				if source.Exists {
					exists = "ready"
				}
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", source.Name, source.Kind, exists, source.Path); err != nil {
					return err
				}
			}
			return nil
		},
	})
	cmd.AddCommand(sourceOneCommand(stdout, root, operatorURL, jsonOutput, "fetch"))
	cmd.AddCommand(sourceOneCommand(stdout, root, operatorURL, jsonOutput, "status"))
	cmd.AddCommand(sourceOneCommand(stdout, root, operatorURL, jsonOutput, "pull"))
	cmd.AddCommand(sourcePushCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

func sourceOneCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool, action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <name>",
		Short: action + " a source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			var state api.SourceState
			switch action {
			case "fetch":
				state, err = platform.SourceFetch(cmd.Context(), args[0])
			case "status":
				state, err = platform.SourceStatus(cmd.Context(), args[0])
			case "pull":
				state, err = platform.SourcePull(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, state)
			}
			exists := "missing"
			if state.Exists {
				exists = "ready"
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", state.Name, state.Kind, exists, state.Path)
			return err
		},
	}
}

func sourcePushCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "push <name>",
		Short: "push a source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			state, err := platform.SourcePush(cmd.Context(), args[0], ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, state)
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\tready\t%s\n", state.Name, state.Kind, state.Path)
			return err
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "ref to push")
	return cmd
}

func workspaceCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Aliases: []string{"ws"}, Short: "Manage workspaces"}
	cmd.AddCommand(workspaceCreateCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceUpdateCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceListCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceGetCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceStatusCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceDestroyCommand(stdout, root, operatorURL))
	cmd.AddCommand(workspaceLogsCommand(stdout, root, operatorURL))
	cmd.AddCommand(workspaceGitCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspacePushCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceSyncBaseCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceOpenCommand(stdout, root, operatorURL))
	cmd.AddCommand(workspacePreflightCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceSourceCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

func workspaceUpdateCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ttl string
	var inputValues []string
	var overwrite bool
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update workspace metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := parseKeyValues(inputValues)
			if err != nil {
				return err
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.WorkspaceUpdate(cmd.Context(), args[0], api.WorkspaceUpdateRequest{Inputs: inputs, TTL: ttl, Overwrite: overwrite})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, ref)
			}
			_, err = fmt.Fprintf(stdout, "workspace %s updated\n", ref.Name)
			return err
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "", "workspace TTL")
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "workspace input K=V")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace conflicting locally modified template files")
	return cmd
}

func workspaceLogsCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [name]",
		Short: "Show workspace logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, name, err := workspaceTarget(args, root, operatorURL, "logs")
			if err != nil {
				return err
			}
			logs, err := platform.WorkspaceLogs(cmd.Context(), name, follow)
			if err != nil {
				return err
			}
			for line := range logs {
				if _, err := fmt.Fprint(stdout, line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow logs")
	return cmd
}

func workspaceGitCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "git <name>",
		Short: "Show workspace git status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			states, err := platform.WorkspaceGitStatus(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, states)
			}
			for _, state := range states {
				ref := state.CurrentRef
				if ref == "" {
					ref = state.Ref
				}
				stateText := state.State
				if stateText == "" {
					stateText = "clean"
					if state.Dirty {
						stateText = "dirty"
					}
				}
				if state.UnpushedReason != "" {
					stateText += " " + state.UnpushedReason
				}
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", state.Slot, ref, stateText, state.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func workspacePushCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "push <name>",
		Short: "Push workspace git sources",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			states, err := platform.WorkspacePush(cmd.Context(), args[0], ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, states)
			}
			for _, state := range states {
				ref := state.CurrentRef
				if ref == "" {
					ref = state.Ref
				}
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", state.Slot, ref, state.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "ref to push")
	return cmd
}

func workspaceSyncBaseCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var merge bool
	var rebase bool
	cmd := &cobra.Command{
		Use:   "sync-base [name]",
		Short: "Update workspace git sources from their base ref without changing branches",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if merge && rebase {
				return fmt.Errorf("choose only one of --merge or --rebase")
			}
			method := "merge"
			if rebase {
				method = "rebase"
			}
			platform, name, err := workspaceTarget(args, root, operatorURL, "sync-base")
			if err != nil {
				return err
			}
			states, err := platform.WorkspaceSyncBase(cmd.Context(), name, method)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, states)
			}
			for _, state := range states {
				ref := state.CurrentRef
				if ref == "" {
					ref = state.Ref
				}
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", state.Slot, ref, state.State, state.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&merge, "merge", false, "merge the base ref into each workspace branch (default)")
	cmd.Flags().BoolVar(&rebase, "rebase", false, "rebase each workspace branch onto its base ref")
	return cmd
}

func workspaceCreateCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var req api.WorkspaceCreateRequest
	var inputs []string
	cmd := &cobra.Command{
		Use:   "create <name> --template <template>",
		Short: "Create a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			if req.Template == "" {
				return fmt.Errorf("--template is required")
			}
			parsedInputs, err := parseKeyValues(inputs)
			if err != nil {
				return err
			}
			req.Inputs = parsedInputs
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.WorkspaceCreate(cmd.Context(), req)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, ref)
			}
			_, err = fmt.Fprintf(stdout, "workspace %s created at %s\n", ref.Name, ref.Path)
			return err
		},
	}
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "template input K=V")
	cmd.Flags().StringVarP(&req.Template, "template", "t", "", "template ref, URL, or path")
	cmd.Flags().StringVar(&req.TTL, "ttl", "", "workspace TTL")
	cmd.Flags().BoolVar(&req.Sync, "sync", false, "reconcile leftover worktree state from a failed create instead of failing")
	return cmd
}

func workspaceListCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List workspaces",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			refs, _, err := platform.WorkspaceList(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, refs)
			}
			for _, ref := range refs {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", ref.Name, ref.Template, ref.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func workspaceGetCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.WorkspaceGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, ref)
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\t%s\n", ref.Name, ref.Template, ref.Path)
			return err
		},
	}
}

func workspaceStatusCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show full workspace status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, name, err := workspaceStatusTarget(args, root, operatorURL)
			if err != nil {
				return err
			}
			status, err := platform.WorkspaceStatus(cmd.Context(), name)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, status)
			}
			return writeWorkspaceStatus(stdout, status)
		},
	}
}

func workspaceStatusTarget(args []string, root, operatorURL *string) (service.API, string, error) {
	return workspaceTarget(args, root, operatorURL, "status")
}

func workspaceTarget(args []string, root, operatorURL *string, command string) (service.API, string, error) {
	if len(args) == 1 {
		platform, err := localPlatform(root, operatorURL)
		return platform, args[0], err
	}
	if operatorURL != nil && *operatorURL != "" {
		return nil, "", fmt.Errorf("workspace %s requires a workspace name in remote operator mode", command)
	}
	currentRoot, name, ok, err := enclosingWorkspace()
	if err != nil {
		return nil, "", err
	}
	if ok {
		platform, err := service.New(currentRoot)
		return platform, name, err
	}
	return nil, "", fmt.Errorf("workspace %s requires a workspace name unless run from inside ANGEE_ROOT/workspaces/<name>", command)
}

func enclosingWorkspace() (root string, name string, ok bool, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", false, err
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", "", false, err
	}
	for {
		parent := filepath.Dir(dir)
		if filepath.Base(parent) == "workspaces" {
			candidateRoot := filepath.Dir(parent)
			if _, err := os.Stat(filepath.Join(candidateRoot, "angee.yaml")); err == nil {
				return candidateRoot, filepath.Base(dir), true, nil
			}
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	return "", "", false, nil
}

func writeWorkspaceStatus(stdout io.Writer, status api.WorkspaceStatusResponse) error {
	if _, err := fmt.Fprintf(stdout, "workspace\t%s\t%s\n", status.Name, status.State); err != nil {
		return err
	}
	if status.Error != "" {
		if _, err := fmt.Fprintf(stdout, "error\t%s\n", status.Error); err != nil {
			return err
		}
	}
	rows := []struct {
		key   string
		value string
	}{
		{key: "path", value: status.Path},
		{key: "template", value: status.Template},
		{key: "chain_root", value: status.ChainRoot},
		{key: "ttl", value: status.TTL},
	}
	for _, row := range rows {
		if row.value == "" {
			continue
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\n", row.key, row.value); err != nil {
			return err
		}
	}
	if status.TTLExpiresAt != nil {
		if _, err := fmt.Fprintf(stdout, "ttl_expires_at\t%s\n", status.TTLExpiresAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if status.ProcessComposePort > 0 {
		if _, err := fmt.Fprintf(stdout, "process_compose_port\t%d\n", status.ProcessComposePort); err != nil {
			return err
		}
	}
	if status.PlaywrightMCPName != "" {
		if _, err := fmt.Fprintf(stdout, "playwright_mcp_name\t%s\n", status.PlaywrightMCPName); err != nil {
			return err
		}
	}
	if status.PlaywrightMCPURL != "" {
		if _, err := fmt.Fprintf(stdout, "playwright_mcp_url\t%s\n", status.PlaywrightMCPURL); err != nil {
			return err
		}
	}
	if len(status.Sources) > 0 {
		if _, err := fmt.Fprintln(stdout, "sources"); err != nil {
			return err
		}
		for _, source := range status.Sources {
			ref := source.CurrentRef
			if ref == "" {
				ref = source.Ref
			}
			detail := source.State
			if source.UnpushedReason != "" {
				detail += " " + source.UnpushedReason
			}
			if source.Upstream != "" {
				detail += " upstream=" + source.Upstream
			}
			if source.Ahead > 0 || source.Behind > 0 {
				detail += fmt.Sprintf(" ahead=%d behind=%d", source.Ahead, source.Behind)
			}
			if _, err := fmt.Fprintf(stdout, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", source.Slot, source.Source, source.Kind, source.Mode, ref, detail, source.Path); err != nil {
				return err
			}
		}
	}
	if len(status.MountedBy) > 0 {
		if _, err := fmt.Fprintln(stdout, "mounted_by"); err != nil {
			return err
		}
		for _, ref := range status.MountedBy {
			if _, err := fmt.Fprintf(stdout, "  %s\t%s\t%s\t%s\n", ref.Kind, ref.Name, ref.Field, ref.Value); err != nil {
				return err
			}
		}
	}
	if status.InnerStack != nil {
		if _, err := fmt.Fprintf(stdout, "inner_stack\t%s\tservices=%d\tjobs=%d\tworkspaces=%d\n", status.InnerStack.Name, len(status.InnerStack.Services), len(status.InnerStack.Jobs), len(status.InnerStack.Workspaces)); err != nil {
			return err
		}
	}
	if status.InnerError != "" {
		if _, err := fmt.Fprintf(stdout, "inner_error\t%s\n", status.InnerError); err != nil {
			return err
		}
	}
	return nil
}

func workspaceDestroyCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Destroy a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.WorkspaceDestroy(cmd.Context(), args[0], purge); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "workspace %s destroyed\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "remove workspace files")
	return cmd
}

func statusCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show declared stack state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			status, err := platform.StackStatus(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, status)
			}
			_, err = fmt.Fprintf(stdout, "%s\nroot: %s\nservices: %d\njobs: %d\nworkspaces: %d\n", status.Name, status.Root, len(status.Services), len(status.Jobs), len(status.Workspaces))
			return err
		},
	}
}

func internalCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	internalCmd := &cobra.Command{
		Use:    "internal",
		Short:  "Internal development commands",
		Hidden: true,
	}
	stackCmd := &cobra.Command{Use: "stack", Short: "Internal stack commands"}
	stackCmd.AddCommand(&cobra.Command{
		Use:   "compile",
		Short: "Compile runtime backend files without starting processes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			compiled, err := platform.StackCompile(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, compiled)
			}
			text, err := compiled.Text()
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(stdout, text)
			return err
		},
	})
	stackCmd.AddCommand(&cobra.Command{
		Use:   "prepare",
		Short: "Compile and write runtime backend files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			compiled, err := platform.StackPrepare(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, compiled)
			}
			_, err = fmt.Fprintln(stdout, "runtime files prepared")
			return err
		},
	})
	internalCmd.AddCommand(stackCmd)
	return internalCmd
}

func operatorCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                "operator",
		Short:              "Run the Angee operator",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return operator.Execute(cmd.Context(), args, stdout, stderr)
		},
	}
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func WithContext(cmd *cobra.Command, ctx context.Context) *cobra.Command {
	cmd.SetContext(ctx)
	return cmd
}
