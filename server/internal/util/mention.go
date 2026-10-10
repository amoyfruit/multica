package util

import (
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Mention represents a parsed @mention from markdown content.
type Mention struct {
	Type string // "member", "agent", "squad", "issue", or "all"
	ID   string // user_id, agent_id, issue_id, or "all"
}

// MentionRe matches [@Label](mention://type/id) or [Label](mention://issue/id) in markdown.
// The @ prefix is optional to support issue mentions which use [MUL-123](mention://issue/...).
// Uses .+? (non-greedy) instead of [^\]]* so labels containing square brackets
// (e.g. "David[TF]") are matched correctly — the ](mention:// anchor is specific
// enough to prevent over-matching.
var MentionRe = regexp.MustCompile(`\[@?(.+?)\]\(mention://(member|agent|squad|issue|all)/([0-9a-fA-F-]+|all)\)`)

var canonicalMentionRe = regexp.MustCompile("^" + MentionRe.String())

// IsMentionAll returns true if the mention is an @all mention.
func (m Mention) IsMentionAll() bool {
	return m.Type == "all"
}

// ParseMentions extracts deduplicated mentions from markdown content.
// Mentions inside fenced code blocks or inline code spans are ignored — they
// document the mention syntax rather than trigger an agent run. Mentions in
// blockquotes are still returned on purpose.
//
// The walk uses goldmark's CommonMark AST and only considers Link nodes, so
// code spans, fenced blocks (including nested-in-blockquote and CommonMark
// fence-length rules), and multiline code spans are excluded without a
// hand-rolled scanner.
// Each link must also use MentionRe's canonical inline source form and have
// visible label text. Reference links and other Markdown link forms are inert.
func ParseMentions(content string) []Mention {
	source := []byte(content)
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	seen := make(map[string]bool)
	var result []Mention
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		mention, ok := parseMentionLink(link, source)
		if !ok {
			return ast.WalkContinue, nil
		}
		key := mention.Type + ":" + mention.ID
		if seen[key] {
			return ast.WalkContinue, nil
		}
		seen[key] = true
		result = append(result, mention)
		return ast.WalkContinue, nil
	})
	return result
}

func parseMentionLink(link *ast.Link, source []byte) (Mention, bool) {
	if link.Reference != nil || len(link.Title) != 0 {
		return Mention{}, false
	}
	start := link.Pos()
	m := canonicalMentionRe.FindSubmatchIndex(source[start:])
	if m == nil {
		return Mention{}, false
	}
	mention := Mention{Type: string(source[start+m[4] : start+m[5]]), ID: string(source[start+m[6] : start+m[7]])}
	if string(link.Destination) != "mention://"+mention.Type+"/"+mention.ID {
		return Mention{}, false
	}

	var label strings.Builder
	canonical := true
	_ = ast.Walk(link, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if text, ok := n.(*ast.Text); ok {
				// A mention-shaped string inside the label must not validate
				// a different outer link's source form.
				if text.Segment.Stop > start+m[3] {
					canonical = false
				}
				label.Write(text.Segment.Value(source))
			}
		}
		return ast.WalkContinue, nil
	})
	visible := strings.TrimSpace(html.UnescapeString(label.String()))
	visible = strings.TrimSpace(strings.TrimPrefix(visible, "@"))
	return mention, canonical && visible != ""
}

// HasMentionAll returns true if any mention in the slice is an @all mention.
func HasMentionAll(mentions []Mention) bool {
	for _, m := range mentions {
		if m.IsMentionAll() {
			return true
		}
	}
	return false
}
