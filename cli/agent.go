package cli

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"text/tabwriter"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var validAgentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

type agentInitOptions struct {
	Template          string
	WorkspaceTemplate string
	Branch            string
	Overrides         []string
	Secrets           []string
	Ports             []string
	CreateBranches    bool
	Start             bool
	Yes               bool
}

type agentUpdateOptions struct {
	Template          string
	WorkspaceTemplate string
	Ref               string
	Overrides         []string
	Secrets           []string
	Ports             []string
	Restart           bool
	Yes               bool
}

var (
	agentInitOpts   agentInitOptions
	agentUpdateOpts agentUpdateOptions
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agent-backed workspaces",
}

var agentInitCmd = &cobra.Command{
	Use:   "init <agent>",
	Short: "Provision an agent-backed workspace",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		overrides, err := parseKeyValueFlags(agentInitOpts.Overrides, "--override")
		if err != nil {
			return err
		}
		secrets, err := parseKeyValueFlags(agentInitOpts.Secrets, "--secret")
		if err != nil {
			return err
		}
		ports, err := parsePortFlags(agentInitOpts.Ports)
		if err != nil {
			return err
		}
		req := api.AgentInitRequest{
			Name:              args[0],
			Root:              resolveRoot(),
			Template:          agentInitOpts.Template,
			WorkspaceTemplate: agentInitOpts.WorkspaceTemplate,
			Branch:            agentInitOpts.Branch,
			Overrides:         overrides,
			Secrets:           secrets,
			Ports:             ports,
			CreateBranches:    agentInitOpts.CreateBranches,
			Start:             agentInitOpts.Start,
			Yes:               agentInitOpts.Yes,
		}
		return postProvision("/agents/init", req)
	},
}

var agentUpdateCmd = &cobra.Command{
	Use:   "update <agent>",
	Short: "Update an agent from its template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		overrides, err := parseKeyValueFlags(agentUpdateOpts.Overrides, "--override")
		if err != nil {
			return err
		}
		secrets, err := parseKeyValueFlags(agentUpdateOpts.Secrets, "--secret")
		if err != nil {
			return err
		}
		ports, err := parsePortFlags(agentUpdateOpts.Ports)
		if err != nil {
			return err
		}
		req := api.AgentUpdateRequest{
			Name:              args[0],
			Root:              resolveRoot(),
			Template:          agentUpdateOpts.Template,
			WorkspaceTemplate: agentUpdateOpts.WorkspaceTemplate,
			Ref:               agentUpdateOpts.Ref,
			Overrides:         overrides,
			Secrets:           secrets,
			Ports:             ports,
			Restart:           agentUpdateOpts.Restart,
			Yes:               agentUpdateOpts.Yes,
		}
		return postProvision("/agents/"+args[0]+"/update", req)
	},
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agents",
	Args:  cobra.NoArgs,
	RunE:  runAgentList,
}

var agentStartCmd = &cobra.Command{
	Use:   "start <agent>",
	Short: "Start an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/start", nil)
	},
}

var agentStopCmd = &cobra.Command{
	Use:   "stop <agent>",
	Short: "Stop an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/stop", nil)
	},
}

var agentRestartCmd = &cobra.Command{
	Use:   "restart <agent>",
	Short: "Restart an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/restart", api.AgentActionRequest{Name: args[0], Root: resolveRoot()})
	},
}

var agentDestroyCmd = &cobra.Command{
	Use:   "destroy <agent>",
	Short: "Destroy an agent workspace",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/destroy", api.AgentActionRequest{Name: args[0], Root: resolveRoot()})
	},
}

var agentLogsCmd = &cobra.Command{
	Use:   "logs <agent>",
	Short: "Show agent logs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return streamAgentLogs(args[0])
	},
}

var agentChatCmd = &cobra.Command{
	Use:   "chat <agent>",
	Short: "Chat with an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/chat", api.AgentChatRequest{Name: args[0], Root: resolveRoot()})
	},
}

var agentAskCmd = &cobra.Command{
	Use:   "ask <agent> <message>",
	Short: "Send a one-shot message to an agent",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentName(args[0]); err != nil {
			return err
		}
		return postProvision("/agents/"+args[0]+"/ask", api.AgentAskRequest{Name: args[0], Root: resolveRoot(), Message: args[1]})
	},
}

func init() {
	agentInitCmd.Flags().StringVar(&agentInitOpts.Template, "template", "", "Agent template ref")
	agentInitCmd.Flags().StringVar(&agentInitOpts.WorkspaceTemplate, "workspace-template", "", "Workspace template ref")
	agentInitCmd.Flags().StringVar(&agentInitOpts.Branch, "branch", "", "Branch or ref for workspace sources")
	agentInitCmd.Flags().StringArrayVar(&agentInitOpts.Overrides, "override", nil, "Override a source ref: --override source=ref")
	agentInitCmd.Flags().StringArrayVar(&agentInitOpts.Secrets, "secret", nil, "Supply a secret: --secret name=value")
	agentInitCmd.Flags().StringArrayVar(&agentInitOpts.Ports, "port", nil, "Override a port lease: --port name=8120")
	agentInitCmd.Flags().BoolVar(&agentInitOpts.CreateBranches, "create-branches", false, "Create missing branches")
	agentInitCmd.Flags().BoolVar(&agentInitOpts.Start, "start", false, "Start agent services after provisioning")
	agentInitCmd.Flags().BoolVarP(&agentInitOpts.Yes, "yes", "y", false, "Accept defaults and run non-interactively")

	agentUpdateCmd.Flags().StringVar(&agentUpdateOpts.Template, "template", "", "Agent template ref")
	agentUpdateCmd.Flags().StringVar(&agentUpdateOpts.WorkspaceTemplate, "workspace-template", "", "Workspace template ref")
	agentUpdateCmd.Flags().StringVar(&agentUpdateOpts.Ref, "ref", "", "Template or source ref to update to")
	agentUpdateCmd.Flags().StringArrayVar(&agentUpdateOpts.Overrides, "override", nil, "Override a source ref: --override source=ref")
	agentUpdateCmd.Flags().StringArrayVar(&agentUpdateOpts.Secrets, "secret", nil, "Supply a secret: --secret name=value")
	agentUpdateCmd.Flags().StringArrayVar(&agentUpdateOpts.Ports, "port", nil, "Override a port lease: --port name=8120")
	agentUpdateCmd.Flags().BoolVar(&agentUpdateOpts.Restart, "restart", false, "Restart services after update")
	agentUpdateCmd.Flags().BoolVarP(&agentUpdateOpts.Yes, "yes", "y", false, "Accept defaults and run non-interactively")

	agentLogsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	agentLogsCmd.Flags().IntVarP(&logsLines, "lines", "n", 100, "Number of lines to show")

	agentCmd.AddCommand(
		agentInitCmd,
		agentListCmd,
		agentStartCmd,
		agentStopCmd,
		agentRestartCmd,
		agentLogsCmd,
		agentChatCmd,
		agentAskCmd,
		agentUpdateCmd,
		agentDestroyCmd,
	)
}

func validateAgentName(name string) error {
	if !validAgentName.MatchString(name) {
		return fmt.Errorf("invalid agent name %q: must match %s", name, validAgentName.String())
	}
	return nil
}

func runAgentList(cmd *cobra.Command, args []string) error {
	var agents []api.AgentInfo
	if _, err := apiGet("/agents", &agents); err != nil {
		return err
	}
	if outputJSON {
		return nil
	}
	if len(agents) == 0 {
		fmt.Println("No agents found.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tHEALTH\tLIFECYCLE\tROLE")
	for _, agent := range agents {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", agent.Name, agent.Status, agent.Health, agent.Lifecycle, agent.Role)
	}
	return w.Flush()
}

func streamAgentLogs(agent string) error {
	params := url.Values{}
	params.Set("lines", fmt.Sprintf("%d", logsLines))
	if logsFollow {
		params.Set("follow", "true")
	}
	return streamAPIGet(fmt.Sprintf("/agents/%s/logs", agent), params, os.Stdout)
}
