package servicehost

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseGRPCTargetMap reads instance ID to host:port JSON mappings.
func ParseGRPCTargetMap(name, value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}
	var targets map[string]string
	if err := json.Unmarshal([]byte(value), &targets); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	for serviceID, target := range targets {
		if strings.TrimSpace(serviceID) == "" || !validGRPCTarget(target) {
			return nil, fmt.Errorf("%s target for %q must be host:port", name, serviceID)
		}
		targets[serviceID] = strings.TrimSpace(target)
	}
	return targets, nil
}

// RequiredGRPCTarget reads one host:port target.
func RequiredGRPCTarget(name, fallback string) (string, error) {
	value := strings.TrimSpace(getenv(name, fallback))
	if !validGRPCTarget(value) {
		return "", fmt.Errorf("%s must be host:port", name)
	}
	return value, nil
}

func validGRPCTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	host, port, ok := strings.Cut(target, ":")
	return ok && strings.TrimSpace(host) != "" && strings.TrimSpace(port) != ""
}
