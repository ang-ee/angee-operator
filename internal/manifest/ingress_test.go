package manifest

import (
	"strings"
	"testing"
)

func TestDefaultsSetsIngressTypeNone(t *testing.T) {
	stack := &Stack{}

	stack.Defaults()

	if stack.Ingress.Type != "none" {
		t.Fatalf("Ingress.Type = %q, want none", stack.Ingress.Type)
	}
}

func TestValidateRejectsRouteOnLocalService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "routed-local",
		Services: map[string]Service{
			"web": {
				Runtime: RuntimeLocal,
				Command: []string{"npm", "run", "dev"},
				Route:   &Route{Port: 3000},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "route") {
		t.Fatalf("Validate() error = %q, want mention of route", err.Error())
	}
}

func TestValidateAllowsRouteOnContainerService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "routed-container",
		Services: map[string]Service{
			"web": {
				Runtime: RuntimeContainer,
				Image:   "example/web:latest",
				Route:   &Route{Port: 3000},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
