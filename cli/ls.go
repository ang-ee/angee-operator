package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var lsCmd = &cobra.Command{
	Use:     "ls",
	Short:   "List running agents and services",
	Aliases: []string{"list", "ps"},
	RunE:    runLs,
}

func runLs(cmd *cobra.Command, args []string) error {
	var statuses []api.ServiceStatus
	if _, err := apiGet("/status", &statuses); err != nil {
		return err
	}
	if outputJSON {
		return nil
	}

	if len(statuses) == 0 {
		fmt.Println("\n  No services running. Try 'angee up'.")
		return nil
	}

	// Split into agents and services
	var services, agents []api.ServiceStatus
	for _, s := range statuses {
		if s.Type == "agent" {
			agents = append(agents, s)
		} else {
			services = append(services, s)
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

	if len(services) > 0 {
		fmt.Fprintln(w, "\nSERVICES")
		fmt.Fprintln(w, "  NAME\tSTATUS\tHEALTH\tREPLICAS\tDOMAINS")
		for _, s := range services {
			status := colorStatus(s.Status)
			health := colorHealth(s.Health)
			replicas := fmt.Sprintf("%d/%d", s.ReplicasRunning, s.ReplicasDesired)
			domains := ""
			if len(s.Domains) > 0 {
				domains = s.Domains[0]
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
				s.Name, status, health, replicas, domains)
		}
	}

	if len(agents) > 0 {
		fmt.Fprintln(w, "\nAGENTS")
		fmt.Fprintln(w, "  NAME\tSTATUS\tHEALTH")
		for _, a := range agents {
			// Strip "agent-" prefix for display
			displayName := a.Name
			if len(displayName) > 6 && displayName[:6] == "agent-" {
				displayName = displayName[6:]
			}
			status := colorStatus(a.Status)
			health := colorHealth(a.Health)
			fmt.Fprintf(w, "  %s\t%s\t%s\n", displayName, status, health)
		}
	}

	fmt.Fprintln(w)
	w.Flush()
	return nil
}

func colorStatus(s string) string {
	switch s {
	case "running":
		return "\033[32m● running\033[0m"
	case "stopped":
		return "\033[90m○ stopped\033[0m"
	case "error":
		return "\033[31m✗ error\033[0m"
	case "starting":
		return "\033[33m◌ starting\033[0m"
	default:
		return s
	}
}

func colorHealth(s string) string {
	switch s {
	case "healthy":
		return "\033[32mhealthy\033[0m"
	case "unhealthy":
		return "\033[31munhealthy\033[0m"
	default:
		return "\033[90munknown\033[0m"
	}
}
