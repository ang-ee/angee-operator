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

// Inputs settle in order: a condition sees the provided values and the
// answers before it, never a later question's default, and a question that
// does not apply settles to its default.
func TestSettleInputs(t *testing.T) {
	inputs := []api.TemplateInputDescriptor{
		{Name: "early", Question: true, Default: "x", When: "{{ late == 'yes' }}"},
		{Name: "runtime_mode", Question: true, Default: "process"},
		{Name: "operator_home", Question: true, Default: "~", When: "{{ runtime_mode == 'docker' }}"},
		{Name: "late", Question: true, Default: "yes"},
	}
	settled, active := SettleInputs(inputs, map[string]string{"operator_home": "/srv/home"}, nil)
	if active["early"] || active["operator_home"] || settled["operator_home"] != "~" || settled["late"] != "yes" {
		t.Fatalf("process: settled %v, active %v; want early off (late's default is not visible) and operator_home at its default", settled, active)
	}
	provided := map[string]string{"late": "yes", "runtime_mode": "docker", "operator_home": "/srv/home"}
	settled, active = SettleInputs(inputs, provided, provided)
	if !active["early"] || !active["operator_home"] || settled["operator_home"] != "/srv/home" {
		t.Fatalf("docker: settled %v, active %v; want early on (late was provided) and operator_home answered", settled, active)
	}
}
