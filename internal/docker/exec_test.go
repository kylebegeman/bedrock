package docker

import (
	"slices"
	"strings"
	"testing"
)

func TestExecNamesEnvironmentVariablesWithoutTheirValues(t *testing.T) {
	args := execArgs("quark-loom-postgres", map[string]string{"PGPASSWORD": "hunter2", "B": "x"}, true, []string{"psql", "-c", "select 1"})
	want := []string{"exec", "-i", "-e", "B", "-e", "PGPASSWORD", "quark-loom-postgres", "psql", "-c", "select 1"}
	if !slices.Equal(args, want) {
		t.Fatalf("args %v, want %v", args, want)
	}
	if strings.Contains(strings.Join(args, " "), "hunter2") {
		t.Fatal("a value reached the arguments")
	}
	env := execEnv(map[string]string{"PGPASSWORD": "hunter2"})
	if !slices.Contains(env, "PGPASSWORD=hunter2") {
		t.Fatal("the value must reach docker's environment")
	}
	if execEnv(nil) != nil {
		t.Fatal("no variables keeps the default environment")
	}
}
