package metrics

import "strings"

// matchesName reports whether name matches any pattern (exact or * suffix wildcard).
func matchesName(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if name == pattern {
			return true
		}
	}
	return false
}

func ruleNeedsPodAPIFilter(rule KubeMetricsRule) bool {
	return rule.LabelSelector != "" || rule.FieldSelector != ""
}

func podKey(namespace, name string) string {
	return namespace + "/" + name
}
