package executor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

func TestExecutors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"config_provider_returns_list_in_order", func(t *testing.T) {
			first := NewLocalExecutor(&defs.ExecutorDefinition{ID: "a"})
			second := NewLocalExecutor(&defs.ExecutorDefinition{ID: "b"})
			provider := NewConfigExecutorProvider([]Executor{first, second})
			for _, id := range []defs.ConversationID{"", "conv-1"} {
				executors, err := provider.GetExecutors(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(executors, []Executor{first, second}) {
					t.Fatalf("executors = %v, want [first second]", executors)
				}
			}
		}},
		{"local_opens_workspace_at_root_directory", func(t *testing.T) {
			root := t.TempDir()
			executor := NewLocalExecutor(&defs.ExecutorDefinition{ID: "local", Kind: defs.ExecutorKindLocal})
			opened, err := executor.NewWorkspace(ctx, &NewWorkspaceRequest{RootDirectory: root})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if err := opened.WriteFile(ctx, filepath.Join(root, "a.txt"), []byte("hi"), 0o644, false); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(root, "a.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(content); got != "hi" {
				t.Fatalf("got %q, want %q", got, "hi")
			}
		}},
		{"virtual_opens_a_new_workspace_each_time", func(t *testing.T) {
			definition := &defs.ExecutorDefinition{ID: "virtual", Kind: defs.ExecutorKindVirtual, WorkspaceDirectory: "/workspace"}
			executor := NewVirtualExecutor(NewVirtualExecutorOpt(definition, map[string][]byte{"/workspace/seed.txt": []byte("seed")}))
			first, err := executor.NewWorkspace(ctx, &NewWorkspaceRequest{})
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			file, err := first.ReadFile(ctx, "/workspace/seed.txt", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(file.Content); got != "seed" {
				t.Fatalf("got %q, want %q", got, "seed")
			}
			if err := first.WriteFile(ctx, "/workspace/new.txt", []byte("x"), 0o644, false); err != nil {
				t.Fatal(err)
			}
			second, err := executor.NewWorkspace(ctx, &NewWorkspaceRequest{})
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			_, err = second.ReadFile(ctx, "/workspace/new.txt", 0, 100)
			if err == nil {
				t.Fatal("error = nil, want an error")
			}
		}},
		{"memory_maps_user_and_project", func(t *testing.T) {
			memoryDirectory := t.TempDir()
			executor := NewMemoryExecutor(NewMemoryExecutorOpt(&defs.ExecutorDefinition{ID: defs.MemoryExecutorID, Kind: defs.ExecutorKindMemory}, memoryDirectory))
			opened, err := executor.NewWorkspace(ctx, &NewWorkspaceRequest{MemoryProjectName: "projects/local/-tmp"})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			memoryWorkspace, ok := opened.(workspace.MemoryWorkspace)
			if !ok {
				t.Fatalf("workspace %T is not a MemoryWorkspace", opened)
			}
			if got := memoryWorkspace.GetProjectName(); got != "projects/local/-tmp" {
				t.Fatalf("got %q, want %q", got, "projects/local/-tmp")
			}
			if err := os.MkdirAll(filepath.Join(memoryDirectory, "user"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := opened.WriteFile(ctx, "user/MEMORY.md", []byte("m"), 0o644, false); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(memoryDirectory, "user", "MEMORY.md"))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(content); got != "m" {
				t.Fatalf("got %q, want %q", got, "m")
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}
