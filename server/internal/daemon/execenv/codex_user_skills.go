package execenv

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// seedUserCodexSkills copies user-installed skill directories from the shared
// ~/.codex/skills/ into the per-task CODEX_HOME so the codex CLI discovers
// them natively. Codex is the only runtime whose HOME is redirected to a
// per-task directory (via the CODEX_HOME env var), so without this step the
// CLI never sees the user's `~/.codex/skills/` content.
//
// Workspace-assigned skills take precedence on name conflict: any user skill
// whose sanitized name matches a workspace skill's sanitized name is skipped
// here, and writeSkillFiles then writes the workspace version into a clean
// slot.
//
// Per-skill failures are logged and skipped — a single broken user skill
// must not prevent the task from running. Returning an error is reserved for
// failures that prevent listing the shared skills directory at all.
func seedUserCodexSkills(codexHome string, workspaceSkills []SkillContextForEnv, logger *slog.Logger) error {
	sharedSkillsDir := filepath.Join(resolveSharedCodexHome(), "skills")

	info, err := os.Stat(sharedSkillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat shared skills dir: %w", err)
	}
	if !info.IsDir() {
		return nil
	}

	reserved := make(map[string]struct{}, len(workspaceSkills))
	for _, s := range workspaceSkills {
		reserved[sanitizeSkillName(s.Name)] = struct{}{}
	}

	entries, err := os.ReadDir(sharedSkillsDir)
	if err != nil {
		return fmt.Errorf("read shared skills dir: %w", err)
	}

	targetSkillsDir := filepath.Join(codexHome, "skills")
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if _, claimed := reserved[sanitizeSkillName(name)]; claimed {
			logger.Info("execenv: codex user-skill yields to workspace skill", "name", name)
			continue
		}
		src := filepath.Join(sharedSkillsDir, name)
		// Installers like lark-cli ship each skill as a symlink into a
		// shared ~/.agents/skills/<name>/ directory. Resolve symlinks so we
		// copy the real content into the per-task home.
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			logger.Warn("execenv: codex user-skill resolve failed", "name", name, "error", err)
			continue
		}
		fi, err := os.Stat(resolved)
		if err != nil || !fi.IsDir() {
			continue
		}
		dst := filepath.Join(targetSkillsDir, name)
		if err := os.RemoveAll(dst); err != nil {
			logger.Warn("execenv: codex user-skill clean dst failed", "name", name, "error", err)
			continue
		}
		if err := copyDirTree(resolved, dst); err != nil {
			logger.Warn("execenv: codex user-skill copy failed", "name", name, "error", err)
			continue
		}
		if err := normalizeCopiedCodexUserSkill(dst, name); err != nil {
			_ = os.RemoveAll(dst)
			logger.Warn("execenv: codex user-skill frontmatter normalization failed", "name", name, "error", err)
			continue
		}
	}
	return nil
}

// normalizeCopiedCodexUserSkill is the cold-start guard for user-installed
// Codex skills. Unlike workspace skills, user skills arrive from ~/.codex/skills
// and used to be copied byte-for-byte into the per-task CODEX_HOME. A missing
// or malformed frontmatter block makes Codex reject that SKILL.md during startup,
// which can stall unrelated task prompts before they produce a fallback answer.
func normalizeCopiedCodexUserSkill(skillDir, name string) error {
	path := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	normalized, err := normalizeUserCodexSkillContent(string(data), sanitizeSkillName(name))
	if err != nil {
		return err
	}
	if normalized == string(data) {
		return nil
	}
	if err := os.WriteFile(path, []byte(normalized), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	return nil
}

// copyDirTree walks src recursively and copies every regular file under it
// to the matching path under dst. Nested symlinks are ignored to keep the
// per-task home self-contained; the caller is expected to resolve the root
// before calling.
func copyDirTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

// normalizeUserCodexSkillContent fills Codex's required string fields while
// retaining policy metadata. Unparseable YAML is skipped rather than dropping
// author-supplied restrictions in a synthetic replacement.
func normalizeUserCodexSkillContent(content, slug string) (string, error) {
	fm, body, ok := frontmatterParts(content)
	description := "Instructions for the " + slug + " skill."
	if !ok {
		if strings.HasPrefix(content, "---") {
			return "", fmt.Errorf("unterminated skill frontmatter")
		}
		return synthesizeFrontmatter(content, slug, description), nil
	}
	var metadata map[string]any
	if err := yaml.Unmarshal([]byte(fm), &metadata); err != nil || metadata == nil {
		return "", fmt.Errorf("invalid skill frontmatter")
	}
	name, _ := metadata["name"].(string)
	desc, _ := metadata["description"].(string)
	if strings.TrimSpace(name) != "" && strings.TrimSpace(desc) != "" {
		return content, nil
	}
	if strings.TrimSpace(name) == "" {
		metadata["name"] = slug
	}
	if strings.TrimSpace(desc) == "" {
		metadata["description"] = description
	}
	encoded, err := yaml.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return "---\n" + string(encoded) + "---\n" + body, nil
}
