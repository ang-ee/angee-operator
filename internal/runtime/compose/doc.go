package compose

import "gopkg.in/yaml.v3"

type File struct {
	Name     string             `yaml:"name,omitempty"`
	Services map[string]Service `yaml:"services,omitempty"`
	Volumes  map[string]Volume  `yaml:"volumes,omitempty"`
	Networks map[string]Network `yaml:"networks,omitempty"`
}

type Service struct {
	Image           string                       `yaml:"image,omitempty"`
	Build           any                          `yaml:"build,omitempty"`
	Command         []string                     `yaml:"command,omitempty"`
	Environment     map[string]string            `yaml:"environment,omitempty"`
	Labels          map[string]string            `yaml:"labels,omitempty"`
	Ports           []string                     `yaml:"ports,omitempty"`
	Volumes         []ServiceVolume              `yaml:"volumes,omitempty"`
	ExtraHosts      []string                     `yaml:"extra_hosts,omitempty" json:"extra_hosts,omitempty"`
	Networks        []string                     `yaml:"networks,omitempty"`
	WorkingDir      string                       `yaml:"working_dir,omitempty"`
	Healthcheck     *Healthcheck                 `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	DependsOn       map[string]ServiceDependency `yaml:"depends_on,omitempty"`
	StopGracePeriod string                       `yaml:"stop_grace_period,omitempty"`
}

type Healthcheck struct {
	Test        []string `yaml:"test" json:"test"`
	Interval    string   `yaml:"interval" json:"interval"`
	Timeout     string   `yaml:"timeout" json:"timeout"`
	Retries     int      `yaml:"retries" json:"retries"`
	StartPeriod string   `yaml:"start_period" json:"start_period"`
}

type ServiceDependency struct {
	Condition string `yaml:"condition,omitempty"`
}

type Volume struct {
	Driver string `yaml:"driver,omitempty"`
	Name   string `yaml:"name,omitempty"`
}

type Network struct {
	External bool `yaml:"external,omitempty"`
}

func Marshal(file File) ([]byte, error) {
	return yaml.Marshal(file)
}

// ServiceVolume is one entry of a service's volumes: Docker's short syntax
// ("source:target[:ro]"), or a read-only bind in the long syntax with
// create_host_path: false. The short syntax makes Docker create a missing
// bind source as an empty root-owned directory; a read-only bind of a missing
// path is never useful, so Docker refuses it instead.
type ServiceVolume struct {
	Short string
	Bind  *ReadOnlyBind
}

// ReadOnlyBind is a read-only bind of an existing host path.
type ReadOnlyBind struct {
	Source string
	Target string
}

// ShortVolume is a volume entry in the short syntax.
func ShortVolume(spec string) ServiceVolume {
	return ServiceVolume{Short: spec}
}

type longVolume struct {
	Type     string         `yaml:"type"`
	Source   string         `yaml:"source"`
	Target   string         `yaml:"target"`
	ReadOnly bool           `yaml:"read_only"`
	Bind     longVolumeBind `yaml:"bind"`
}

type longVolumeBind struct {
	CreateHostPath bool `yaml:"create_host_path"`
}

func (v ServiceVolume) MarshalYAML() (any, error) {
	if v.Bind == nil {
		return v.Short, nil
	}
	return longVolume{Type: "bind", Source: v.Bind.Source, Target: v.Bind.Target, ReadOnly: true}, nil
}

func (v *ServiceVolume) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*v = ShortVolume(node.Value)
		return nil
	}
	var long longVolume
	if err := node.Decode(&long); err != nil {
		return err
	}
	*v = ServiceVolume{Bind: &ReadOnlyBind{Source: long.Source, Target: long.Target}}
	return nil
}
