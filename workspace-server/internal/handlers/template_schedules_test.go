package handlers

// template_schedules_test.go — unit tests for parseTemplateSchedules.
//
// seedTemplateSchedules' DB INSERT path is already covered indirectly
// by TestImport_OrgScheduleSQLShape (schedules_test.go) since both
// code paths share the canonical orgImportScheduleSQL constant; the
// loop logic (default tz, default enabled, prompt resolution, cron
// validation) is exercised at the parser level here and at the
// orgImportScheduleSQL level there.

import (
	"path/filepath"
	"testing"
)

func TestParseTemplateSchedules_AbsentFile(t *testing.T) {
	dir := t.TempDir()
	// No config.yaml in dir.
	got, err := parseTemplateSchedules(dir)
	if err != nil {
		t.Fatalf("expected nil error for absent config.yaml, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice, got %#v", got)
	}
}

func TestParseTemplateSchedules_EmptyTemplatePath(t *testing.T) {
	got, err := parseTemplateSchedules("")
	if err != nil {
		t.Fatalf("expected nil error for empty path, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil slice for empty path, got %#v", got)
	}
}

func TestParseTemplateSchedules_NoSchedulesBlock(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: Some Template
runtime: claude-code
model: foo/bar
`)
	got, err := parseTemplateSchedules(dir)
	if err != nil {
		t.Fatalf("expected nil error when schedules: absent, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero schedules, got %d", len(got))
	}
}

func TestParseTemplateSchedules_HappyPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: SEO Agent
schedules:
  - name: Continuous tick
    cron_expr: "*/30 * * * *"
    timezone: America/Vancouver
    prompt: |
      Run one SEO tick.
  - name: Monday GSC
    cron_expr: "0 8 * * 1"
    timezone: America/Vancouver
    prompt: /seo google
    enabled: true
  - name: Disabled placeholder
    cron_expr: "0 0 1 1 *"
    prompt: noop
    enabled: false
`)
	got, err := parseTemplateSchedules(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 schedules, got %d", len(got))
	}
	if got[0].Name != "Continuous tick" || got[0].CronExpr != "*/30 * * * *" {
		t.Errorf("schedule[0] mismatch: %+v", got[0])
	}
	if got[1].Timezone != "America/Vancouver" {
		t.Errorf("schedule[1].Timezone = %q, want America/Vancouver", got[1].Timezone)
	}
	// Enabled is *bool: nil means "default true" at seed time, false is
	// explicit opt-out and must survive the YAML round-trip.
	if got[2].Enabled == nil {
		t.Errorf("schedule[2].Enabled = nil, want *false")
	} else if *got[2].Enabled {
		t.Errorf("schedule[2].Enabled = true, want false")
	}
}

func TestParseTemplateSchedules_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), `
name: Broken
schedules:
  - this is: [not, a, valid
`)
	_, err := parseTemplateSchedules(dir)
	if err == nil {
		t.Fatal("expected parse error on malformed YAML, got nil")
	}
}

// TestParseTemplateSchedules_RejectsOversizeFile gates against the
// billion-laughs / anchor-bomb DoS class: a hostile config.yaml over
// the 1 MiB cap must be refused before yaml.Unmarshal runs.
func TestParseTemplateSchedules_RejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	// One byte over the cap — fastest path to the gate.
	pad := make([]byte, maxTemplateConfigYAMLBytes+1)
	for i := range pad {
		pad[i] = '#'
	}
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), string(pad))
	if _, err := parseTemplateSchedules(dir); err == nil {
		t.Fatal("expected oversize-file error, got nil")
	}
}

// TestParseTemplateSchedules_RejectsTooManySchedules gates against a
// hostile config.yaml that flips one row into a 10k-row insert storm.
func TestParseTemplateSchedules_RejectsTooManySchedules(t *testing.T) {
	dir := t.TempDir()
	var b []byte
	b = append(b, []byte("schedules:\n")...)
	// maxTemplateSchedules+1 minimal entries — they don't have to be
	// valid as schedules because the gate trips before resolution.
	for i := 0; i <= maxTemplateSchedules; i++ {
		b = append(b, []byte("  - name: s\n    cron_expr: \"* * * * *\"\n    prompt: x\n")...)
	}
	mustWriteFile(t, filepath.Join(dir, "config.yaml"), string(b))
	if _, err := parseTemplateSchedules(dir); err == nil {
		t.Fatal("expected schedule-count error, got nil")
	}
}
