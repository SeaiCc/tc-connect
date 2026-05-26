package core

import (
	"strings"
	"fmt"
)

func parseUserLocalReference(raw, workspaceDir string) (*localReference, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty reference")
	}
	if match := reMarkdownLink.FindStringSubmatch(raw); len(match) >= 3 && match[0] == raw {
		suffix := ""
		if len(match) >= 4 {
			suffix = match[3]
		}
		raw = match[2] + suffix
	}
	ref, ok := parseLocalReference(raw, workspaceDir)
	if !ok {
		return nil, fmt.Errorf("cannot parse local reference")
	}
	return ref, nil
}
