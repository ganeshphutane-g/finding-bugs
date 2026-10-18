package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a length of time written as 30s, 5m or a number of seconds.
type Duration time.Duration

// UnmarshalYAML reads 30s, 1m30s or 90.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a duration such as 30s or 5m", n.Line)
	}
	if secs, err := strconv.Atoi(n.Value); err == nil && secs >= 0 {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: %q is not a duration such as 30s or 5m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }
