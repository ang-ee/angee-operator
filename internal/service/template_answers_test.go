package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeQuestionsStackTemplate is writeStackTemplate with more copier questions.
func writeQuestionsStackTemplate(t *testing.T, project, questions, manifestBody string) {
	t.Helper()
	dir := writeStackTemplate(t, project, manifestBody)
	path := filepath.Join(dir, "copier.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(questions)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A validator copier runs on an answer, here on the default it uses, fails the
// render as INVALID_INPUT with the validator's message.
func TestStackInitReportsValidatorFailureAsInvalidInput(t *testing.T) {
	project := t.TempDir()
	writeQuestionsStackTemplate(t, project, `serve_mode:
  type: str
  default: development
runtime_mode:
  type: str
  default: process
  validator: "{% if serve_mode == 'production' and runtime_mode == 'process' %}production serving needs runtime_mode docker{% endif %}"
`, oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.StackInit(context.Background(), "dev", "", map[string]string{"ANGEE_ROOT": ".angee", "serve_mode": "production"}, false)
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeInvalidInput || !strings.Contains(err.Error(), "production serving needs runtime_mode docker") || !strings.Contains(err.Error(), "runtime_mode") {
		t.Fatalf("StackInit() error = %v (%+v), want INVALID_INPUT naming runtime_mode and the validator's message", err, opErr)
	}
}

// Defaults are left to copier: a templated default is rendered, not passed on
// as its literal text, and the invalid default of a question whose `when` is
// false is not validated.
func TestStackInitLeavesDefaultsToCopier(t *testing.T) {
	project := t.TempDir()
	writeQuestionsStackTemplate(t, project, `name:
  type: str
  default: Demo
slug:
  type: str
  default: "{{ name|lower }}"
runtime_mode:
  type: str
  default: process
operator_home:
  type: str
  default: relative
  when: "{{ runtime_mode == 'docker' }}"
  validator: "{% if operator_home == 'relative' %}operator_home must be absolute{% endif %}"
`, `version: 1
kind: stack
name: "{{ slug }}"
template:
  active: stacks/dev
  answers_file: .copier-answers.yml
services:
  web:
    runtime: container
    image: nginx:latest
`)
	p, err := New(project)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.StackInit(context.Background(), "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	if err != nil {
		t.Fatalf("StackInit() error = %v, want the inactive question's default not validated", err)
	}
	stack, err := New(result.Root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := stack.LoadStack()
	if err != nil || loaded.Name != "demo" {
		t.Fatalf("stack name = %q (%v), want the templated default rendered to demo", loaded.Name, err)
	}
}

// A template that needs a newer Copier than angee implements is a
// precondition the template sets, not the user's input.
func TestStackInitReportsTemplateNeedingNewerCopier(t *testing.T) {
	project := t.TempDir()
	writeQuestionsStackTemplate(t, project, "_min_copier_version: \"99.0.0\"\n", oneServiceTemplate)
	p, err := New(project)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.StackInit(context.Background(), "dev", "", map[string]string{"ANGEE_ROOT": ".angee"}, false)
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseTemplateTooNew || opErr.Hint == "" {
		t.Fatalf("StackInit() error = %v (%+v), want PRECONDITION_FAILED template_too_new", err, opErr)
	}
}
