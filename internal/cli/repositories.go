package cli

import (
	"encoding/json"
	"fmt"

	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

// repositoryFlags are the flags that fill a stack template's repository list
// inputs: each is sugar for `--input <input>=<JSON array>`. The CLI only packs
// what was given; the service reads, checks and normalizes the entries, so a
// list reads the same from a flag, `--input`, an answers file or the API.
var repositoryFlags = []struct{ flag, input, help string }{
	// No backquotes: pflag would take the quoted word for the value's name.
	{"repository", service.RepositoriesInput, "repository the stack runs, as <repo>[@<ref>]: a URL, git@host:owner/name.git, an absolute path, or a GitHub owner/name (repeatable; fills the template's repositories input)"},
	{"extra-source", service.ExtraSourcesInput, "repository the stack only checks out, as <repo>[@<ref>] (repeatable; fills the template's extra_sources input)"},
}

// addRepositoryFlags declares repositoryFlags on cmd, each help prefixed with
// prefix (a condition such as "with --template, ").
func addRepositoryFlags(cmd *cobra.Command, prefix string) {
	for _, f := range repositoryFlags {
		cmd.Flags().StringArray(f.flag, nil, prefix+f.help)
	}
}

// repositoryFlagsGiven reports whether cmd was given one of repositoryFlags.
func repositoryFlagsGiven(cmd *cobra.Command) bool {
	for _, f := range repositoryFlags {
		if cmd.Flags().Changed(f.flag) {
			return true
		}
	}
	return false
}

// repositoryFlagInputs returns inputs with every repository flag given on cmd
// packed into its template input, as a JSON array of the strings given. A
// flag given together with `--input` for the same input is refused: the flag
// replaces the list, so one of the two would be dropped.
func repositoryFlagInputs(cmd *cobra.Command, inputs map[string]string) (map[string]string, error) {
	for _, f := range repositoryFlags {
		values, err := cmd.Flags().GetStringArray(f.flag)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			continue
		}
		if _, both := inputs[f.input]; both {
			return nil, fmt.Errorf("--%s and --input %s=… both give the template input %q; give the list one way", f.flag, f.input, f.input)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		if inputs == nil {
			inputs = map[string]string{}
		}
		inputs[f.input] = string(encoded)
	}
	return inputs, nil
}
