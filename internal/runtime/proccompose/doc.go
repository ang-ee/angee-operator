package proccompose

import "gopkg.in/yaml.v3"

type File struct {
	Version   string             `yaml:"version"`
	Processes map[string]Process `yaml:"processes,omitempty"`
}

type Process struct {
	Command        string                       `yaml:"command,omitempty"`
	Environment    []string                     `yaml:"environment,omitempty"`
	WorkingDir     string                       `yaml:"working_dir,omitempty"`
	ReadinessProbe *Probe                       `yaml:"readiness_probe,omitempty" json:"readiness_probe,omitempty"`
	DependsOn      map[string]ProcessDependency `yaml:"depends_on,omitempty"`
}

type Probe struct {
	HTTPGet             *HTTPGet   `yaml:"http_get,omitempty" json:"http_get,omitempty"`
	Exec                *ExecProbe `yaml:"exec,omitempty" json:"exec,omitempty"`
	InitialDelaySeconds int        `yaml:"initial_delay_seconds" json:"initial_delay_seconds"`
	PeriodSeconds       int        `yaml:"period_seconds" json:"period_seconds"`
	TimeoutSeconds      int        `yaml:"timeout_seconds" json:"timeout_seconds"`
	FailureThreshold    int        `yaml:"failure_threshold" json:"failure_threshold"`
}

type HTTPGet struct {
	Host   string `yaml:"host" json:"host"`
	Port   string `yaml:"port" json:"port"`
	Path   string `yaml:"path" json:"path"`
	Scheme string `yaml:"scheme" json:"scheme"`
}

type ExecProbe struct {
	Command string `yaml:"command" json:"command"`
}

type ProcessDependency struct {
	Condition string `yaml:"condition,omitempty"`
}

func Marshal(file File) ([]byte, error) {
	return yaml.Marshal(file)
}
