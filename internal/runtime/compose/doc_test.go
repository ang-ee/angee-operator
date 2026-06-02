package compose

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMarshalNetworksAndLabelsRoundTrip(t *testing.T) {
	file := File{
		Services: map[string]Service{
			"web": {
				Image:    "caddy:latest",
				Labels:   map[string]string{"caddy": "route"},
				Networks: []string{"edge"},
			},
		},
		Networks: map[string]Network{
			"edge": {},
		},
	}

	data, err := Marshal(file)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	text := string(data)
	if strings.Count(text, "networks:") != 2 {
		t.Fatalf("Marshal() networks count = %d, want 2\n%s", strings.Count(text, "networks:"), text)
	}
	if !strings.Contains(text, "labels:") {
		t.Fatalf("Marshal() output missing labels:\n%s", text)
	}
	if !strings.Contains(text, "edge: {}") {
		t.Fatalf("Marshal() output missing empty network object:\n%s", text)
	}

	var got File
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(got, file) {
		t.Fatalf("round trip = %#v, want %#v", got, file)
	}
}

func TestMarshalOmitsNetworksAndLabels(t *testing.T) {
	file := File{
		Services: map[string]Service{
			"web": {
				Image: "nginx:latest",
			},
		},
	}

	data, err := Marshal(file)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	text := string(data)
	if strings.Contains(text, "networks:") {
		t.Fatalf("Marshal() output unexpectedly contains networks:\n%s", text)
	}
	if strings.Contains(text, "labels:") {
		t.Fatalf("Marshal() output unexpectedly contains labels:\n%s", text)
	}
}
