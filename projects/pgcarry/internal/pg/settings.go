package pg

import (
	"context"
	"strings"
)

// Setting is a server setting and where it came from. Value is as SHOW
// gives it, units included ("8MB"), so it can be set again as it is.
type Setting struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Source     string `json:"source"`
	SourceFile string `json:"sourcefile,omitempty"`
}

// FileSettings lists settings that come from configuration files inside
// the data directory (postgresql.conf, postgresql.auto.conf), which a dump
// does not carry.
func (s *Server) FileSettings(ctx context.Context, db, dataDir string) ([]Setting, error) {
	var list []Setting
	err := s.queryJSON(ctx, db, `select coalesce(json_agg(json_build_object(
		'name', name, 'value', current_setting(name), 'source', source,
		'sourcefile', coalesce(sourcefile, '')) order by name), '[]')
		from pg_settings where source = 'configuration file'`, &list)
	if err != nil {
		return nil, err
	}
	var in []Setting
	for _, st := range list {
		if dataDir == "" || strings.HasPrefix(st.SourceFile, strings.TrimSuffix(dataDir, "/")+"/") {
			in = append(in, st)
		}
	}
	return in, nil
}

// File returns a file from inside the container ("" if it cannot be read).
func (s *Server) File(ctx context.Context, path string) string {
	out, err := s.CLI.Run(ctx, "exec", s.Container, "cat", path)
	if err != nil {
		return ""
	}
	return out
}

// RulesOf returns the non-comment, non-empty lines of a config file, with
// whitespace normalised.
func RulesOf(file string) []string {
	var out []string
	for _, line := range strings.Split(file, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, strings.Join(f, " "))
		}
	}
	return out
}
