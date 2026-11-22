package executor_test

import (
	"slices"
	"sync"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
)

func TestExecutionEnvironmentsManagerOrder(t *testing.T) {
	for _, dynamicIDs := range [][]string{{"b", "a"}, {"a", "b"}} {
		t.Run(dynamicIDs[0]+"_first", func(t *testing.T) {
			manager := executor.NewExecutionEnvironmentsManager()
			static := []executor.ExecutionEnvironment{newEchoEnvironment(t, "z"), newEchoEnvironment(t, "local")}
			if _, err := manager.SetStatic("z", static); err != nil {
				t.Fatal(err)
			}
			static[0] = nil
			for _, id := range dynamicIDs {
				if err := manager.SetDynamic(newDynamicEnvironment(t, id), id+"-1"); err != nil {
					t.Fatal(err)
				}
			}
			environments, defaultID := manager.GetEnvironments()
			if got := getEnvironmentIDs(environments); defaultID != "z" || !slices.Equal(got, []string{"z", "local", "a", "b"}) {
				t.Fatalf("GetEnvironments() = %v, %q", got, defaultID)
			}
			environments[0], environments[2] = nil, nil
			environments, _ = manager.GetEnvironments()
			if got := getEnvironmentIDs(environments); !slices.Equal(got, []string{"z", "local", "a", "b"}) {
				t.Fatalf("GetEnvironments() after mutating returned slice = %v", got)
			}
		})
	}
}

func TestExecutionEnvironmentsManagerSetStatic(t *testing.T) {
	local := newEchoEnvironment(t, "local")
	other := newEchoEnvironment(t, "other")
	dynamic := newDynamicEnvironment(t, "other")
	for _, tt := range []struct {
		name         string
		defaultID    string
		environments []executor.ExecutionEnvironment
	}{
		{name: "nil environment", environments: []executor.ExecutionEnvironment{nil}},
		{name: "dynamic environment", defaultID: "other", environments: []executor.ExecutionEnvironment{dynamic}},
		{name: "duplicate ID", defaultID: "local", environments: []executor.ExecutionEnvironment{local, local}},
		{name: "empty default", environments: []executor.ExecutionEnvironment{local}},
		{name: "default not first", defaultID: "other", environments: []executor.ExecutionEnvironment{local, other}},
		{name: "default without environments", defaultID: "local"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager := newStaticEnvironmentsManager(t, local)
			if err := manager.SetDynamic(dynamic, "other-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.SetStatic(tt.defaultID, tt.environments); err == nil {
				t.Fatal("SetStatic() succeeded for invalid configuration")
			}
			environments, defaultID := manager.GetEnvironments()
			if defaultID != "local" || !slices.Equal(environments, []executor.ExecutionEnvironment{local, dynamic}) {
				t.Fatalf("invalid SetStatic changed environments to %v, default %q", environments, defaultID)
			}
		})
	}

	t.Run("static configuration removes colliding dynamic environments", func(t *testing.T) {
		manager := newStaticEnvironmentsManager(t, local)
		for _, id := range []string{"other", "remote"} {
			if err := manager.SetDynamic(newDynamicEnvironment(t, id), id+"-1"); err != nil {
				t.Fatal(err)
			}
		}
		removed, err := manager.SetStatic("other", []executor.ExecutionEnvironment{other})
		if err != nil || !slices.Equal(removed, []string{"other"}) {
			t.Fatalf("SetStatic() = %v, %v, want [other], nil", removed, err)
		}
		if environment, found := manager.GetEnvironment("other"); !found || environment != other || manager.RemoveDynamic("other", "other-1") {
			t.Fatalf("GetEnvironment(other) = %v, %t, want static environment", environment, found)
		}
		if _, err := manager.SetStatic("", nil); err != nil {
			t.Fatal(err)
		}
		environments, defaultID := manager.GetEnvironments()
		if got := getEnvironmentIDs(environments); defaultID != "" || !slices.Equal(got, []string{"remote"}) {
			t.Fatalf("GetEnvironments() after clearing static environments = %v, %q", got, defaultID)
		}
	})
}

func TestExecutionEnvironmentsManagerSetDynamic(t *testing.T) {
	local := newEchoEnvironment(t, "local")
	for _, tt := range []struct {
		name         string
		environment  executor.ExecutionEnvironment
		connectionID string
	}{
		{name: "nil environment", connectionID: "connection"},
		{name: "static environment", environment: local, connectionID: "connection"},
		{name: "missing connection ID", environment: newDynamicEnvironment(t, "remote")},
		{name: "reserved ID", environment: newDynamicEnvironment(t, defs.MemoryExecutorID), connectionID: "connection"},
		{name: "static ID", environment: newDynamicEnvironment(t, "local"), connectionID: "connection"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager := newStaticEnvironmentsManager(t, local)
			if err := manager.SetDynamic(tt.environment, tt.connectionID); err == nil {
				t.Fatal("SetDynamic() succeeded for invalid registration")
			}
			environments, defaultID := manager.GetEnvironments()
			if defaultID != "local" || !slices.Equal(environments, []executor.ExecutionEnvironment{local}) {
				t.Fatalf("invalid SetDynamic changed environments to %v, default %q", environments, defaultID)
			}
		})
	}

	t.Run("old connection cannot remove replacement", func(t *testing.T) {
		manager := executor.NewExecutionEnvironmentsManager()
		first, replacement := newDynamicEnvironment(t, "remote"), newDynamicEnvironment(t, "remote")
		if err := manager.SetDynamic(first, "first"); err != nil {
			t.Fatal(err)
		}
		if err := manager.SetDynamic(replacement, "replacement"); err != nil {
			t.Fatal(err)
		}
		if manager.RemoveDynamic("remote", "first") {
			t.Fatal("RemoveDynamic removed the replacement")
		}
		if environment, found := manager.GetEnvironment("remote"); !found || environment != replacement {
			t.Fatalf("GetEnvironment(remote) = %v, %t, want replacement", environment, found)
		}
		if !manager.RemoveDynamic("remote", "replacement") || manager.RemoveDynamic("remote", "replacement") {
			t.Fatal("RemoveDynamic should remove the matching registration once")
		}
		if _, found := manager.GetEnvironment("remote"); found {
			t.Fatal("GetEnvironment(remote) found the removed environment")
		}
	})
}

func TestExecutionEnvironmentsManagerConcurrentReads(t *testing.T) {
	local, alternate := newEchoEnvironment(t, "local"), newEchoEnvironment(t, "alternate")
	remote := newDynamicEnvironment(t, "remote")
	manager := newStaticEnvironmentsManager(t, local)
	var tasks sync.WaitGroup
	for range 4 {
		tasks.Go(func() {
			for range 100 {
				environments, defaultID := manager.GetEnvironments()
				if len(environments) == 0 || environments[0].GetID() != defaultID {
					t.Errorf("GetEnvironments() returned a mismatched default ID %q for %v", defaultID, environments)
					return
				}
				if environment, found := manager.GetEnvironment("remote"); found && environment != remote {
					t.Errorf("GetEnvironment(remote) = %v, want registered dynamic environment", environment)
					return
				}
			}
		})
	}
	for range 100 {
		for _, static := range []executor.ExecutionEnvironment{alternate, local} {
			if _, err := manager.SetStatic(static.GetID(), []executor.ExecutionEnvironment{static}); err != nil {
				t.Error(err)
			}
			if err := manager.SetDynamic(remote, "connection"); err != nil {
				t.Error(err)
			}
			if !manager.RemoveDynamic(remote.GetID(), "connection") {
				t.Error("RemoveDynamic failed to remove the matching connection")
			}
		}
	}
	tasks.Wait()
}

func newStaticEnvironmentsManager(t *testing.T, environments ...executor.ExecutionEnvironment) executor.ExecutionEnvironmentsManager {
	t.Helper()
	manager := executor.NewExecutionEnvironmentsManager()
	if len(environments) > 0 {
		if _, err := manager.SetStatic(environments[0].GetID(), environments); err != nil {
			t.Fatal(err)
		}
	}
	return manager
}

func newDynamicEnvironment(t *testing.T, id string, connections ...mcpclient.Connection) executor.ExecutionEnvironment {
	t.Helper()
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, id+" 环境", nil).
		WithoutCommands().AsDynamic().WithMCPConnections(connections...))
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func getEnvironmentIDs(environments []executor.ExecutionEnvironment) []string {
	ids := make([]string, 0, len(environments))
	for _, environment := range environments {
		ids = append(ids, environment.GetID())
	}
	return ids
}
