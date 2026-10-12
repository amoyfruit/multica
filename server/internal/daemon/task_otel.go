package daemon

import "strings"

func injectTaskOTELResourceAttributes(agentEnv map[string]string, inheritedAttributes, taskID string) {
	const envKey = "OTEL_RESOURCE_ATTRIBUTES"
	const taskKey = "multica.task.id"

	attributes, configured := agentEnv[envKey]
	if !configured {
		attributes = inheritedAttributes
	}
	parts := make([]string, 0)
	for _, attribute := range strings.Split(attributes, ",") {
		key, _, _ := strings.Cut(attribute, "=")
		if strings.TrimSpace(key) == taskKey || strings.TrimSpace(attribute) == "" {
			continue
		}
		parts = append(parts, attribute)
	}
	// Replace stale or custom task IDs so telemetry identifies this execution.
	parts = append(parts, taskKey+"="+taskID)
	agentEnv[envKey] = strings.Join(parts, ",")
}
