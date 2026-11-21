package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type dailyPromptOutcome struct {
	SessionID        string `json:"sessionId"`
	Title            string `json:"title"`
	Lens             string `json:"lens"`
	PromptVersion    string `json:"promptVersion"`
	State            string `json:"state"`
	PlanScore        int    `json:"planScore"`
	PlanStatus       string `json:"planStatus"`
	SessionGateCount int    `json:"sessionGateCount"`
	PRCreated        bool   `json:"prCreated"`
	AuditStatus      string `json:"auditStatus"`
	CIGreen          bool   `json:"ciGreen"`
	Merged           bool   `json:"merged"`
	Failed           bool   `json:"failed"`
}

type dailyVariantMetric struct {
	Sessions      int     `json:"sessions"`
	Plans10       int     `json:"plans10"`
	SessionGates  int     `json:"sessionGates"`
	PRs           int     `json:"pullRequests"`
	PendingAudits int     `json:"pendingAudits"`
	CIGreen       int     `json:"ciGreen"`
	Merged        int     `json:"merged"`
	Failed        int     `json:"failed"`
	PRYieldRate   float64 `json:"prYieldRate"`
	SuccessRate   float64 `json:"successRate"`
}

type dailyPromptMetricReport struct {
	GeneratedAt        string                        `json:"generatedAt,omitempty"`
	TotalSessions      int                           `json:"totalSessions"`
	TotalPRs           int                           `json:"totalPullRequests"`
	PendingAudits      int                           `json:"pendingAudits"`
	TotalMerged        int                           `json:"totalMerged"`
	OverallPRYieldRate float64                       `json:"overallPrYieldRate"`
	OverallSuccessRate float64                       `json:"overallSuccessRate"`
	ByVariant          map[string]dailyVariantMetric `json:"byVariant"`
	Outcomes           []dailyPromptOutcome          `json:"outcomes,omitempty"`
}

func runDailyMetrics(
	ctx context.Context,
	client *julesClient,
	args []string,
) error {
	flags := flag.NewFlagSet("daily-metrics", flag.ContinueOnError)
	output := flags.String("output", "", "relatório JSON de eficácia")
	finalizationDir := flags.String(
		"finalization-dir",
		"",
		"diretório privado de recibos de finalização",
	)
	auditDir := flags.String(
		"audit-dir",
		"",
		"árvore privada de auditorias 30/30",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output é obrigatório")
	}
	finalizedSessions, err := loadFinalizedSessionIDs(*finalizationDir)
	if err != nil {
		return err
	}
	auditStatuses, err := loadDailyAuditStatuses(*auditDir)
	if err != nil {
		return err
	}
	sessions, err := listAllSessions(ctx, client)
	if err != nil {
		return err
	}
	outcomes := make([]dailyPromptOutcome, 0, len(sessions))
	// A métrica usa a mesma janela móvel do ledger e do reaper. Cortar estados
	// terminais em 2 horas fazia um canário 3/3 desaparecer antes da decisão.
	metricCutoff := nowUTC().Add(-dailyQuotaWindow)
	for _, session := range sessions {
		if !strings.HasPrefix(session.Title, dailyTitlePrefix) {
			continue
		}
		lastChange := session.UpdateTime
		if lastChange.IsZero() {
			lastChange = session.CreateTime
		}
		if isTerminalJulesState(session.State) &&
			!lastChange.IsZero() &&
			lastChange.Before(metricCutoff) {
			continue
		}
		outcome := dailyPromptOutcome{
			SessionID:     session.ID,
			Title:         session.Title,
			Lens:          lensFromTitle(session.Title),
			PromptVersion: promptVersionFromSession(session),
			State:         session.State,
			PRCreated:     sessionPullRequestURL(session.Raw) != "",
			Failed:        session.State == julesStateFailed,
		}
		if _, finalized := finalizedSessions[session.ID]; finalized {
			outcome.AuditStatus = "finalized"
		} else if auditStatus, audited := auditStatuses[session.ID]; audited {
			outcome.AuditStatus = auditStatus
		} else if outcome.PRCreated {
			outcome.AuditStatus = "pending"
		} else {
			outcome.AuditStatus = "not_available"
		}
		activities, requestErr := listAllPlanSignals(ctx, client, session.ID)
		if requestErr != nil {
			if errors.Is(requestErr, errJulesResponseTooLarge) {
				outcome.PlanStatus = "activity_too_large"
				outcomes = append(outcomes, outcome)
				continue
			}
			return requestErr
		}
		metric, metricErr := measureLatestAutoPRPlan(session, activities)
		if metricErr != nil {
			return metricErr
		}
		outcome.PlanScore = metric.Score
		outcome.PlanStatus = "measured"
		outcome.SessionGateCount = len(metric.HardGates)
		outcomes = append(outcomes, outcome)
	}
	report := aggregatePromptMetrics(outcomes)
	report.GeneratedAt = nowUTC().Format(time.RFC3339)
	report.Outcomes = outcomes
	return writePrivateJSON(*output, report)
}

func loadDailyAuditStatuses(dir string) (map[string]string, error) {
	statuses := make(map[string]string)
	if dir == "" {
		return statuses, nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return statuses, nil
	} else if err != nil {
		return nil, fmt.Errorf("consultar diretório de auditorias: %w", err)
	}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "audit-") ||
			!strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		var audit dailyPRAudit
		if err := readDailyFinalizationPrivateJSON(path, &audit); err != nil {
			return err
		}
		if err := validateDailyPRAudit(audit, audit.Author); err != nil {
			return fmt.Errorf("auditoria %s inválida: %w", path, err)
		}
		if existing, duplicate := statuses[audit.SessionID]; duplicate &&
			existing != audit.Disposition {
			return fmt.Errorf(
				"sessão %s possui auditorias conflitantes: %s e %s",
				audit.SessionID,
				existing,
				audit.Disposition,
			)
		}
		statuses[audit.SessionID] = audit.Disposition
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ler árvore de auditorias: %w", err)
	}
	return statuses, nil
}

func aggregatePromptMetrics(
	outcomes []dailyPromptOutcome,
) dailyPromptMetricReport {
	report := dailyPromptMetricReport{
		ByVariant: make(map[string]dailyVariantMetric),
	}
	for _, outcome := range outcomes {
		version := outcome.PromptVersion
		if version == "" {
			version = "unknown"
		}
		lens := outcome.Lens
		if lens == "" {
			lens = "unknown"
		}
		key := version + "/" + lens
		metric := report.ByVariant[key]
		metric.Sessions++
		if outcome.PlanScore == 10 {
			metric.Plans10++
		}
		if outcome.SessionGateCount > 0 {
			metric.SessionGates++
		}
		if outcome.PRCreated {
			metric.PRs++
			report.TotalPRs++
		}
		if outcome.AuditStatus == "pending" {
			metric.PendingAudits++
			report.PendingAudits++
		}
		if outcome.CIGreen {
			metric.CIGreen++
		}
		if outcome.Merged {
			metric.Merged++
		}
		if outcome.Failed {
			metric.Failed++
		}
		metric.PRYieldRate = float64(metric.PRs) / float64(metric.Sessions)
		metric.SuccessRate = float64(metric.Merged) / float64(metric.Sessions)
		report.ByVariant[key] = metric
		report.TotalSessions++
		if outcome.Merged {
			report.TotalMerged++
		}
	}
	if report.TotalSessions > 0 {
		report.OverallPRYieldRate =
			float64(report.TotalPRs) / float64(report.TotalSessions)
		report.OverallSuccessRate =
			float64(report.TotalMerged) / float64(report.TotalSessions)
	}
	return report
}
