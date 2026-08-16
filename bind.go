package cf_observability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bind is one or more host:port listen addresses. JSON/YAML is a string for
// a single listener (":9090") or an array for several (ports may differ).
type Bind []string

// UnmarshalJSON accepts a string or an array of strings.
func (b *Bind) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*b = nil
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		return b.set([]string{s})
	}
	var ss []string
	if err := json.Unmarshal(data, &ss); err != nil {
		return fmt.Errorf("cf_observability: bind must be a string or array of host:port: %w", err)
	}
	return b.set(ss)
}

// MarshalJSON writes a string when there is one address, otherwise an array.
func (b Bind) MarshalJSON() ([]byte, error) {
	if len(b) == 0 {
		return []byte("null"), nil
	}
	if len(b) == 1 {
		return json.Marshal(b[0])
	}
	return json.Marshal([]string(b))
}

// UnmarshalYAML accepts a scalar or a sequence of host:port strings.
func (b *Bind) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Tag == "!!null" || (value.Kind == yaml.ScalarNode && value.Value == "") {
		*b = nil
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		return b.set([]string{value.Value})
	}
	var ss []string
	if err := value.Decode(&ss); err != nil {
		return fmt.Errorf("cf_observability: bind must be a string or array of host:port: %w", err)
	}
	return b.set(ss)
}

func (b *Bind) set(ss []string) error {
	out, err := parseBindList(ss)
	if err != nil {
		return err
	}
	*b = out
	return nil
}

func parseBindList(ss []string) (Bind, error) {
	if len(ss) == 0 {
		return nil, fmt.Errorf("cf_observability: bind array must not be empty")
	}
	seen := make(map[string]struct{}, len(ss))
	out := make(Bind, 0, len(ss))
	for _, raw := range ss {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, fmt.Errorf("cf_observability: bind entry is empty")
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return nil, fmt.Errorf("cf_observability: bind %q: want host:port (empty host is all interfaces): %w", s, err)
		}
		if port == "" {
			return nil, fmt.Errorf("cf_observability: bind %q: port is required", s)
		}
		canon := net.JoinHostPort(host, port)
		if _, ok := seen[canon]; ok {
			return nil, fmt.Errorf("cf_observability: duplicate bind %q", canon)
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	return out, nil
}

func listenAll(addrs []string) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(addrs))
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, x := range lns {
				_ = x.Close()
			}
			return nil, fmt.Errorf("listen %s: %w", a, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

func pickBoundAddr(lns []net.Listener) string {
	var first string
	for _, ln := range lns {
		s := ln.Addr().String()
		if first == "" {
			first = s
		}
		host, _, err := net.SplitHostPort(s)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return s
		}
	}
	return first
}

func bindsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
