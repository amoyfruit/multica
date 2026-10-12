package daemon

import (
	"maps"
	"strings"
	"testing"
)

func TestInjectTaskOTELResourceAttributes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		inherited string
		customEnv map[string]string
		want      string
	}{
		{
			name: "no existing attributes",
			want: "multica.task.id=task-current",
		},
		{
			name:      "preserve inherited attributes",
			inherited: "service.name=copilot,deployment.environment.name=production",
			want:      "service.name=copilot,deployment.environment.name=production,multica.task.id=task-current",
		},
		{
			name:      "custom attributes take precedence",
			inherited: "service.name=daemon",
			customEnv: map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=agent,team=blue"},
			want:      "service.name=agent,team=blue,multica.task.id=task-current",
		},
		{
			name:      "explicit empty custom attributes",
			inherited: "service.name=daemon",
			customEnv: map[string]string{"OTEL_RESOURCE_ATTRIBUTES": ""},
			want:      "multica.task.id=task-current",
		},
		{
			name:      "replace inherited task id",
			inherited: "multica.task.id=task-old,service.name=agent",
			want:      "service.name=agent,multica.task.id=task-current",
		},
		{
			name: "replace duplicate custom task ids",
			customEnv: map[string]string{
				"OTEL_RESOURCE_ATTRIBUTES": "multica.task.id=wrong,service.name=agent, multica.task.id =also-wrong",
			},
			want: "service.name=agent,multica.task.id=task-current",
		},
		{
			name:      "preserve encoded values and equals signs",
			inherited: "label=a%2Cb,query=a=b,other.task.id=other",
			want:      "label=a%2Cb,query=a=b,other.task.id=other,multica.task.id=task-current",
		},
		{
			name:      "ignore empty separators",
			inherited: ", ,service.name=agent,,",
			want:      "service.name=agent,multica.task.id=task-current",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"MULTICA_TASK_ID": "task-current"}
			originalCustom := maps.Clone(tt.customEnv)
			layerCustomEnvAndHermesHome(env, tt.customEnv, "", nil)
			injectTaskOTELResourceAttributes(env, tt.inherited, env["MULTICA_TASK_ID"])
			if got := env["OTEL_RESOURCE_ATTRIBUTES"]; got != tt.want {
				t.Fatalf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", got, tt.want)
			}
			if env["MULTICA_TASK_ID"] != "task-current" {
				t.Fatal("task ID environment variable changed")
			}
			if !maps.Equal(tt.customEnv, originalCustom) {
				t.Fatal("agent custom environment was mutated")
			}
			injectTaskOTELResourceAttributes(env, tt.inherited, "task-current")
			if got := env["OTEL_RESOURCE_ATTRIBUTES"]; got != tt.want {
				t.Fatalf("repeated injection = %q, want %q", got, tt.want)
			}
			injectTaskOTELResourceAttributes(env, tt.inherited, "task-next")
			if got, want := env["OTEL_RESOURCE_ATTRIBUTES"], strings.TrimSuffix(tt.want, "task-current")+"task-next"; got != want {
				t.Fatalf("next task injection = %q, want %q", got, want)
			}
		})
	}
}
