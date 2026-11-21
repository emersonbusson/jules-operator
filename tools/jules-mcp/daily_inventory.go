package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var dailyExcludedLabels = []string{
	"epic",
	"blocked",
	"blocked:infra",
	"deferred",
	"discussion",
}

type dailyExcludedIssue struct {
	Issue  dailyIssue `json:"issue"`
	Reason string     `json:"reason"`
}

type dailyIssueInventory struct {
	GeneratedAt   string               `json:"generatedAt"`
	OpenCount     int                  `json:"openCount"`
	EligibleCount int                  `json:"eligibleCount"`
	ExcludedCount int                  `json:"excludedCount"`
	Filters       []string             `json:"filters"`
	Eligible      []dailyIssue         `json:"eligible"`
	Excluded      []dailyExcludedIssue `json:"excluded"`
}

func runDailyIssueInventory(args []string) error {
	flags := flag.NewFlagSet("daily-issue-inventory", flag.ContinueOnError)
	issuesPath := flags.String("issues", "", "JSON dos issues abertos")
	jsonOutput := flags.String("json-output", "", "inventário JSON")
	markdownOutput := flags.String("markdown-output", "", "inventário Markdown")
	generatedAt := flags.String(
		"generated-at",
		time.Now().UTC().Format(time.RFC3339),
		"instante RFC3339 do snapshot",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *issuesPath == "" || *jsonOutput == "" || *markdownOutput == "" {
		return errors.New(
			"--issues, --json-output e --markdown-output são obrigatórios",
		)
	}
	if _, err := time.Parse(time.RFC3339, *generatedAt); err != nil {
		return errors.New("--generated-at deve usar RFC3339")
	}
	var issues []dailyIssue
	if err := readJSONFile(*issuesPath, &issues); err != nil {
		return err
	}
	inventory := buildDailyIssueInventory(issues, *generatedAt)
	if err := writePrivateJSON(*jsonOutput, inventory); err != nil {
		return err
	}
	return writePrivateText(
		*markdownOutput,
		[]byte(renderDailyIssueInventory(inventory)),
	)
}

func buildDailyIssueInventory(
	issues []dailyIssue,
	generatedAt string,
) dailyIssueInventory {
	eligible, excluded := partitionDailyIssues(issues)
	filters := append([]string(nil), dailyExcludedLabels...)
	filters = append(filters, "ci_scope")
	return dailyIssueInventory{
		GeneratedAt:   generatedAt,
		OpenCount:     len(issues),
		EligibleCount: len(eligible),
		ExcludedCount: len(excluded),
		Filters:       filters,
		Eligible:      eligible,
		Excluded:      excluded,
	}
}

func partitionDailyIssues(
	issues []dailyIssue,
) ([]dailyIssue, []dailyExcludedIssue) {
	seen := make(map[int]struct{}, len(issues))
	eligible := make([]dailyIssue, 0, len(issues))
	excluded := make([]dailyExcludedIssue, 0)
	for _, issue := range issues {
		reason := dailyIssueExclusionReason(issue, seen)
		if issue.Number > 0 {
			seen[issue.Number] = struct{}{}
		}
		if reason != "" {
			excluded = append(excluded, dailyExcludedIssue{
				Issue:  issue,
				Reason: reason,
			})
			continue
		}
		eligible = append(eligible, issue)
	}
	sort.SliceStable(eligible, func(left, right int) bool {
		return eligible[left].Number > eligible[right].Number
	})
	sort.SliceStable(excluded, func(left, right int) bool {
		return excluded[left].Issue.Number > excluded[right].Issue.Number
	})
	return eligible, excluded
}

func dailyIssueExclusionReason(
	issue dailyIssue,
	seen map[int]struct{},
) string {
	if issue.Number <= 0 {
		return "invalid_number"
	}
	if strings.TrimSpace(issue.Title) == "" {
		return "missing_title"
	}
	if _, duplicate := seen[issue.Number]; duplicate {
		return "duplicate_number"
	}
	labels := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		labels[strings.ToLower(strings.TrimSpace(label.Name))] = struct{}{}
	}
	if isDailyCIScopedIssue(issue, labels) {
		return "ci_scope"
	}
	for _, blocked := range dailyExcludedLabels {
		if _, exists := labels[blocked]; exists {
			return blocked
		}
	}
	return ""
}

func isDailyCIScopedIssue(
	issue dailyIssue,
	labels map[string]struct{},
) bool {
	for label := range labels {
		if label == "ci" || label == "ci-cd" ||
			strings.HasPrefix(label, "ci-") ||
			strings.HasPrefix(label, "ci:") {
			return true
		}
	}

	title := strings.ToLower(issue.Title)
	if containsAny(
		title,
		"workflow",
		"github actions",
		"ci",
		"runner",
	) || containsTextToken(title, "ci") {
		return true
	}

	body := strings.ToLower(issue.Body)
	return containsAny(
		body,
		".github/workflows/",
		"github actions",
		"actions/cache",
		"workflow_dispatch",
		"runs-on:",
		"ci",
	)
}

func containsTextToken(value, expected string) bool {
	for _, token := range strings.FieldsFunc(value, func(character rune) bool {
		return (character < 'a' || character > 'z') &&
			(character < '0' || character > '9')
	}) {
		if token == expected {
			return true
		}
	}
	return false
}

func renderDailyIssueInventory(inventory dailyIssueInventory) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# Inventário diário de issues do Jules\n\n")
	fmt.Fprintf(&report, "Gerado em `%s` a partir dos issues abertos do GitHub.\n\n", inventory.GeneratedAt)
	fmt.Fprintf(&report, "## Resumo\n\n")
	fmt.Fprintf(
		&report,
		"- %d issues abertos;\n- %d elegíveis;\n- %d excluído(s).\n\n",
		inventory.OpenCount,
		inventory.EligibleCount,
		inventory.ExcludedCount,
	)
	fmt.Fprintf(
		&report,
		"Filtros de exclusão: `%s`.\n\n",
		strings.Join(inventory.Filters, "`, `"),
	)
	fmt.Fprintf(&report, "## Elegíveis (%d)\n\n", inventory.EligibleCount)
	fmt.Fprintln(&report, "| Issue | Título | Labels | Atualizado |")
	fmt.Fprintln(&report, "| --- | --- | --- | --- |")
	for _, issue := range inventory.Eligible {
		fmt.Fprintf(
			&report,
			"| [#%d](%s) | %s | %s | %s |\n",
			issue.Number,
			canonicalIssueURL(issue.Number),
			escapeMarkdownCell(issue.Title),
			escapeMarkdownCell(issueLabelNames(issue)),
			escapeMarkdownCell(issue.UpdatedAt),
		)
	}
	fmt.Fprintf(&report, "\n## Excluídos (%d)\n\n", inventory.ExcludedCount)
	fmt.Fprintln(&report, "| Issue | Título | Labels | Motivo | Atualizado |")
	fmt.Fprintln(&report, "| --- | --- | --- | --- | --- |")
	for _, item := range inventory.Excluded {
		fmt.Fprintf(
			&report,
			"| [#%d](%s) | %s | %s | `%s` | %s |\n",
			item.Issue.Number,
			canonicalIssueURL(item.Issue.Number),
			escapeMarkdownCell(item.Issue.Title),
			escapeMarkdownCell(issueLabelNames(item.Issue)),
			escapeMarkdownCell(item.Reason),
			escapeMarkdownCell(item.Issue.UpdatedAt),
		)
	}
	return report.String()
}

func canonicalIssueURL(number int) string {
	return "https://github.com/emersonbusson/ramshared/issues/" + strconv.Itoa(number)
}

func issueLabelNames(issue dailyIssue) string {
	names := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if name := strings.TrimSpace(label.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func escapeMarkdownCell(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"|", "\\|",
		"[", "\\[",
		"]", "\\]",
		"*", "\\*",
		"_", "\\_",
		"`", "\\`",
		"<", "&lt;",
		">", "&gt;",
		"\r", " ",
		"\n", " ",
	)
	return replacer.Replace(value)
}

func writePrivateText(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criar diretório de artefato: %w", err)
	}
	temp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("gravar artefato temporário: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("publicar artefato: %w", err)
	}
	return nil
}
