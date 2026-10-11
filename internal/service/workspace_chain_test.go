package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/query"
)

// The workspace-chain refusals of #110: a workspace template with `chain` or
// `chain_root` is refused, and a stack template's own `chain:` is not.
func TestRefuseWorkspaceChain(t *testing.T) {
	chain := []copierx.ChainEntry{{Template: "stacks/dev", Root: ".angee"}}
	tests := []struct {
		name     string
		metadata copierx.Metadata
		refused  bool
	}{
		{name: "workspace without a chain", metadata: copierx.Metadata{Kind: "workspace"}},
		{name: "workspace with chain", metadata: copierx.Metadata{Kind: "workspace", Chain: chain}, refused: true},
		{name: "workspace with chain_root only", metadata: copierx.Metadata{Kind: "workspace", ChainRoot: ".angee"}, refused: true},
		{name: "stack with chain", metadata: copierx.Metadata{Kind: "stack", Chain: chain, ChainRoot: "."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RefuseWorkspaceChain("workspaces/x", tt.metadata)
			if !tt.refused {
				if err != nil {
					t.Fatalf("RefuseWorkspaceChain = %v, want nil", err)
				}
				return
			}
			var invalid *InvalidInputError
			if !errors.As(err, &invalid) || invalid.Field != "template" || !strings.Contains(err.Error(), "#110") || !strings.Contains(err.Error(), "workspace://") {
				t.Fatalf("RefuseWorkspaceChain = %v, want an invalid template naming #110 and workspace:// mounts", err)
			}
		})
	}
}

// A workspace template that chains is refused by create, preflight and
// template introspection before anything is rendered or recorded; the
// template list leaves it out. A chaining stack template is still described.
func TestWorkspaceTemplateThatChainsIsRefusedBeforeRendering(t *testing.T) {
	for name, chainLines := range map[string]string{
		"chain":      "  chain:\n    - template: stacks/dev\n      root: .angee\n",
		"chain_root": "  chain_root: .angee\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			platform, err := New(root)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := manifest.SaveFile(manifest.Path(root), platform.EmptyStack("host")); err != nil {
				t.Fatalf("SaveFile: %v", err)
			}
			writeChainingWorkspaceTemplate(t, root, chainLines)
			stackTemplate := filepath.Join(root, ".templates", "stacks", "composite")
			mustWriteFileAll(t, filepath.Join(stackTemplate, "copier.yml"), "_subdirectory: template\n_angee:\n  kind: stack\n  name: composite\n  chain:\n    - template: ../../projects/web\n      root: .\n")
			before, err := os.ReadFile(manifest.Path(root))
			if err != nil {
				t.Fatalf("ReadFile(angee.yaml): %v", err)
			}

			assertRefused := func(what string, err error) {
				t.Helper()
				var invalid *InvalidInputError
				if !errors.As(err, &invalid) || invalid.Field != "template" || !strings.Contains(err.Error(), "#110") {
					t.Fatalf("%s error = %v, want the #110 refusal", what, err)
				}
			}
			_, err = platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: "workspaces/chaining", Name: "feature"})
			assertRefused("WorkspaceCreate", err)
			if _, err := os.Stat(filepath.Join(root, "workspaces", "feature")); !os.IsNotExist(err) {
				t.Fatalf("workspace directory exists after a refused create (stat error %v)", err)
			}
			if after, _ := os.ReadFile(manifest.Path(root)); string(after) != string(before) {
				t.Fatalf("angee.yaml changed after a refused create:\n%s", after)
			}
			_, err = platform.WorkspaceCreatePreflight(ctx, api.WorkspaceCreateRequest{Template: "workspaces/chaining"})
			assertRefused("WorkspaceCreatePreflight", err)
			_, err = platform.Template(ctx, "workspaces/chaining")
			assertRefused("Template", err)

			if _, err := platform.Template(ctx, "stacks/composite"); err != nil {
				t.Fatalf("Template(chaining stack template) = %v, want it described", err)
			}
			listed, _, err := platform.Templates(ctx, query.Args{})
			if err != nil {
				t.Fatalf("Templates: %v", err)
			}
			refs := make([]string, 0, len(listed))
			for _, descriptor := range listed {
				refs = append(refs, descriptor.Ref)
			}
			if got := strings.Join(refs, ","); got != "stacks/composite" {
				t.Fatalf("Templates refs = %q, want only the stack template (the chaining workspace template left out)", got)
			}
		})
	}
}

// Updating a workspace whose template has since gained a chain is refused, and
// nothing in the workspace or the stack changes.
func TestWorkspaceUpdateRefusesATemplateThatNowChains(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspaceTemplate := writeRerenderWorkspaceTemplate(t, base, "outer v1\n")
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	created, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature"})
	if err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	copierPath := filepath.Join(workspaceTemplate, "copier.yml")
	copier, err := os.ReadFile(copierPath)
	if err != nil {
		t.Fatalf("ReadFile(copier.yml): %v", err)
	}
	mustWriteFile(t, copierPath, string(copier)+"  chain_root: .angee\n")
	mustWriteFile(t, filepath.Join(workspaceTemplate, "template", "outer.txt.jinja"), "outer v2\n")
	before, _ := os.ReadFile(manifest.Path(root))

	_, err = platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{})
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "#110") {
		t.Fatalf("WorkspaceUpdate error = %v, want the #110 refusal", err)
	}
	if outer, _ := os.ReadFile(filepath.Join(created.Path, "outer.txt")); string(outer) != "outer v1\n" {
		t.Fatalf("outer.txt = %q after a refused update, want it unchanged", outer)
	}
	if after, _ := os.ReadFile(manifest.Path(root)); string(after) != string(before) {
		t.Fatal("angee.yaml changed after a refused update")
	}
}

// A workspace recorded with a chain before #110 still loads. Its status reports
// no chain and no inner stack, though a stack sits at its old chain root, its
// logs end at once, and destroy removes its record.
func TestWorkspaceRecordedWithAChainReportsNoInnerStack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "host",
		Workspaces: map[string]manifest.Workspace{
			"feature": {
				Template: "workspaces/dev-pr",
				Resolved: manifest.WorkspaceResolved{Chain: []string{"workspaces/dev-pr", "stacks/dev"}, ChainRoot: ".angee"},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(host): %v", err)
	}
	inner := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "inner"}
	innerRoot := filepath.Join(root, "workspaces", "feature", ".angee")
	if err := os.MkdirAll(innerRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(inner root): %v", err)
	}
	if err := manifest.SaveFile(manifest.Path(innerRoot), inner); err != nil {
		t.Fatalf("SaveFile(inner): %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	status, err := platform.WorkspaceStatus(ctx, "feature")
	if err != nil {
		t.Fatalf("WorkspaceStatus: %v", err)
	}
	//nolint:staticcheck // checks the deprecated fields stay empty (#110)
	if status.InnerStack != nil || status.InnerError != "" || status.ChainRoot != "" || status.Chain == nil || len(status.Chain) != 0 {
		t.Fatalf("status inner stack %+v, inner error %q, chain %#v, chain root %q; want none, and an empty (not nil) chain", status.InnerStack, status.InnerError, status.Chain, status.ChainRoot) //nolint:staticcheck // as above
	}
	// REST and --json keep sending a chain list until #110 step 2.
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal(status): %v", err)
	}
	if !strings.Contains(string(encoded), `"chain":[]`) {
		t.Fatalf("status JSON = %s, want \"chain\":[]", encoded)
	}
	ref, err := platform.WorkspaceGet(ctx, "feature")
	if err != nil {
		t.Fatalf("WorkspaceGet: %v", err)
	}
	if chainRoot := ref.ChainRoot; chainRoot != "" { //nolint:staticcheck // checks the deprecated field stays empty (#110)
		t.Fatalf("ref chain root = %q, want empty", chainRoot)
	}
	logs, err := platform.WorkspaceLogs(ctx, "feature", true)
	if err != nil {
		t.Fatalf("WorkspaceLogs: %v", err)
	}
	for line := range logs {
		t.Fatalf("WorkspaceLogs sent %q, want a closed stream", line)
	}
	if _, err := platform.WorkspaceLogs(ctx, "missing", false); !errors.As(err, new(*NotFoundError)) {
		t.Fatalf("WorkspaceLogs(missing) error = %v, want not found", err)
	}
	if err := platform.WorkspaceDestroy(ctx, "feature", false); err != nil {
		t.Fatalf("WorkspaceDestroy: %v", err)
	}
	saved, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if _, ok := saved.Workspaces["feature"]; ok {
		t.Fatal("workspace record kept after destroy")
	}
}

// Updating a workspace recorded with a chain before #110 is refused before
// anything renders: a render of the workspace template alone would overwrite
// or delete what the chain rendered. The record and files stay as they were.
// A record listing only the workspace's own template, as every workspace
// created before #110 has, is not a chain.
func TestWorkspaceUpdateRefusesAWorkspaceRecordedWithAChain(t *testing.T) {
	tests := []struct {
		name    string
		record  func(template string) manifest.WorkspaceResolved
		refused bool
	}{
		{name: "chain root and chain", refused: true, record: func(template string) manifest.WorkspaceResolved {
			return manifest.WorkspaceResolved{Chain: []string{template, "stacks/dev"}, ChainRoot: "."}
		}},
		{name: "chain root only", refused: true, record: func(string) manifest.WorkspaceResolved {
			return manifest.WorkspaceResolved{ChainRoot: ".angee"}
		}},
		{name: "chain only", refused: true, record: func(template string) manifest.WorkspaceResolved {
			return manifest.WorkspaceResolved{Chain: []string{template, "stacks/dev"}}
		}},
		{name: "own template only", record: func(template string) manifest.WorkspaceResolved {
			return manifest.WorkspaceResolved{Chain: []string{template}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			root := filepath.Join(base, ".angee")
			workspaceTemplate := writeRerenderWorkspaceTemplate(t, base, "outer v1\n")
			platform, err := New(root)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			created, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: workspaceTemplate, Name: "feature"})
			if err != nil {
				t.Fatalf("WorkspaceCreate: %v", err)
			}
			// What a chain with `root: .` rendered over the workspace root: the
			// stack's own answers file and manifest.
			answers := filepath.Join(created.Path, ".copier-answers.yml")
			mustWriteFile(t, answers, "_src_path: stacks/dev\nproject: inner\n")
			stack, err := manifest.LoadFile(manifest.Path(root))
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			workspace := stack.Workspaces["feature"]
			resolved := tt.record(workspace.Template)
			workspace.Resolved.Chain = resolved.Chain
			workspace.Resolved.ChainRoot = resolved.ChainRoot
			stack.Workspaces["feature"] = workspace
			if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
				t.Fatalf("SaveFile: %v", err)
			}
			mustWriteFile(t, filepath.Join(workspaceTemplate, "template", "outer.txt.jinja"), "outer v2\n")
			before, _ := os.ReadFile(manifest.Path(root))

			_, err = platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{})
			if !tt.refused {
				if err != nil {
					t.Fatalf("WorkspaceUpdate: %v, want it to proceed", err)
				}
				if outer, _ := os.ReadFile(filepath.Join(created.Path, "outer.txt")); string(outer) != "outer v2\n" {
					t.Fatalf("outer.txt = %q, want the update rendered", outer)
				}
				return
			}
			var invalid *InvalidInputError
			if !errors.As(err, &invalid) || invalid.Field != "workspace" || !strings.Contains(err.Error(), "#110") || !strings.Contains(err.Error(), "destroy it and create it again") {
				t.Fatalf("WorkspaceUpdate error = %v, want the #110 refusal asking to destroy and create again", err)
			}
			if data, _ := os.ReadFile(answers); string(data) != "_src_path: stacks/dev\nproject: inner\n" {
				t.Fatalf(".copier-answers.yml = %q after a refused update, want the chain's answers untouched", data)
			}
			if outer, _ := os.ReadFile(filepath.Join(created.Path, "outer.txt")); string(outer) != "outer v1\n" {
				t.Fatalf("outer.txt = %q after a refused update, want it unchanged", outer)
			}
			if after, _ := os.ReadFile(manifest.Path(root)); string(after) != string(before) {
				t.Fatal("angee.yaml changed after a refused update")
			}
		})
	}
}

func mustWriteFileAll(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	mustWriteFile(t, path, contents)
}
