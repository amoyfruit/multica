package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCopiedCodexUserSkill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte("# broken\n\nplain body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := normalizeCopiedCodexUserSkill(dir, "Broken Skill"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.HasPrefix(text, "---\n") || !strings.Contains(text, "name: broken-skill\n") {
		t.Fatalf("normalized SKILL.md = %q", text)
	}
	if !strings.Contains(text, "plain body") {
		t.Fatalf("normalization dropped skill body: %q", text)
	}
}
