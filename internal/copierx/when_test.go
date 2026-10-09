package copierx

import (
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// `when` is evaluated by copier-go with the answers parsed to their types.
func TestInputApplies(t *testing.T) {
	inputs := []api.TemplateInputDescriptor{{Name: "runtime_mode", Type: "str"}, {Name: "use_ssh", Type: "bool"}}
	for _, tc := range []struct {
		when   string
		values map[string]string
		want   bool
	}{
		{when: "", want: true},
		{when: "{{ runtime_mode == 'docker' }}", values: map[string]string{"runtime_mode": "docker"}, want: true},
		{when: "{{ runtime_mode == 'docker' }}", values: map[string]string{"runtime_mode": "process"}, want: false},
		{when: "{{ use_ssh }}", values: map[string]string{"use_ssh": "false"}, want: false},
		{when: "{{ use_ssh }}", values: map[string]string{"use_ssh": "true"}, want: true},
		{when: "false", want: false},
		{when: "{{ broken", want: true},
	} {
		if got := InputApplies(tc.when, inputs, tc.values); got != tc.want {
			t.Errorf("InputApplies(%q, %v) = %v, want %v", tc.when, tc.values, got, tc.want)
		}
	}
}
