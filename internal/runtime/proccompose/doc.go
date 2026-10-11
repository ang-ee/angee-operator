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
	Shutdown       *Shutdown                    `yaml:"shutdown,omitempty" json:"shutdown,omitempty"`
	// LogLocation is a file process-compose writes the process's output to,
	// besides its in-memory log buffer.
	LogLocation string     `yaml:"log_location,omitempty"`
	LogConfig   *LogConfig `yaml:"log_configuration,omitempty"`
}

// LogConfig is process-compose's log_configuration for a LogLocation file.
// Without DisableJSON each line is a JSON object; with it, a text line that
// NoMetadata keeps to the level and message, and AddTimestamp prefixes with the
// time in TimestampFormat (a Go layout; process-compose's default has no date).
type LogConfig struct {
	// FlushEachLine writes every line to the file as it arrives; without it
	// process-compose buffers the file until the process's logger closes.
	FlushEachLine   bool         `yaml:"flush_each_line,omitempty"`
	DisableJSON     bool         `yaml:"disable_json,omitempty"`
	NoColor         bool         `yaml:"no_color,omitempty"`
	NoMetadata      bool         `yaml:"no_metadata,omitempty"`
	AddTimestamp    bool         `yaml:"add_timestamp,omitempty"`
	TimestampFormat string       `yaml:"timestamp_format,omitempty"`
	Rotation        *LogRotation `yaml:"rotation,omitempty"`
}

// LogRotation has process-compose rotate a LogLocation file at MaxSizeMB,
// keeping MaxBackups rotated files beside it.
type LogRotation struct {
	MaxSizeMB  int `yaml:"max_size_mb,omitempty"`
	MaxBackups int `yaml:"max_backups,omitempty"`
}

type Shutdown struct {
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"shutDownTimeout,omitempty"`
	Signal         int `yaml:"signal,omitempty" json:"signal,omitempty"`
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
	// WorkingDir is the probe's own cwd: process-compose runs exec probes in
	// its daemon directory, not the process's working_dir, so relative
	// file and cmd probes must carry the service workdir explicitly.
	WorkingDir string `yaml:"working_dir,omitempty" json:"working_dir,omitempty"`
}

type ProcessDependency struct {
	Condition string `yaml:"condition,omitempty"`
}

func Marshal(file File) ([]byte, error) {
	return yaml.Marshal(file)
}
