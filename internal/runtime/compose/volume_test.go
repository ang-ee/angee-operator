package compose

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A short volume stays a string; a read-only bind becomes the long syntax with
// create_host_path: false, so Docker refuses a missing source. Both read back.
func TestServiceVolumeYAML(t *testing.T) {
	service := Service{Volumes: []ServiceVolume{
		ShortVolume("./data:/data"),
		{Bind: &ReadOnlyBind{Source: "/home/deploy/.ssh", Target: "/home/deploy/.ssh"}},
	}}
	out, err := yaml.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `volumes:
    - ./data:/data
    - type: bind
      source: /home/deploy/.ssh
      target: /home/deploy/.ssh
      read_only: true
      bind:
        create_host_path: false
`
	if string(out) != want {
		t.Fatalf("YAML =\n%s\nwant\n%s", out, want)
	}
	var back Service
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(back.Volumes, service.Volumes) {
		t.Fatalf("round trip = %#v, want %#v", back.Volumes, service.Volumes)
	}
	if strings.Contains(string(out), "create_host_path: true") {
		t.Fatal("a read-only bind must not let Docker create its source")
	}
}
