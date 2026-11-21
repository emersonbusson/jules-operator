package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxOpenJulesInboxPRs = 300
const maxImmediateBatchSize = 50

type dailyDispatchArtifact struct {
	GeneratedAt             string          `json:"generatedAt"`
	Hour                    int             `json:"hour"`
	Requested               int             `json:"requested"`
	Created                 []sessionRecord `json:"created"`
	Recent24Hours           int             `json:"recent24Hours"`
	ActiveBefore            int             `json:"activeBefore"`
	OpenPRsBefore           int             `json:"openPullRequestsBefore"`
	ReservedPRs             int             `json:"reservedPendingPullRequestsBefore"`
	Canary                  bool            `json:"canary"`
	CanaryAttempt           int             `json:"canaryAttempt,omitempty"`
	ImmediateBatchSize      int             `json:"immediateBatchSize,omitempty"`
	UpstreamCapacityReached bool            `json:"upstreamCapacityReached,omitempty"`
	PromptVersion           string          `json:"promptVersion"`
}

func hourlySlots(hour int) (int, error) {
	if hour < 0 || hour > 23 {
		return 0, errors.New("hora deve estar entre 0 e 23")
	}
	if hour%6 == 0 {
		return 5, nil
	}
	return 4, nil
}

func runDailyDispatch(
	ctx context.Context,
	client *julesClient,
	args []string,
	now time.Time,
) error {
	flags := flag.NewFlagSet("daily-dispatch", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "", "manifesto diário")
	output := flags.String("output", "", "artefato do dispatch")
	confirmTarget := flags.Int("confirm-target", -1, "meta Pro autorizada")
	hour := flags.Int("hour", -1, "hora local 0-23")
	promoted := flags.Bool("promoted", false, "canário aprovado pelo operador")
	retryCanary := flags.Bool(
		"retry-canary",
		false,
		"repete uma única vez um canário anterior terminal",
	)
	immediateBatchSize := flags.Int(
		"immediate-batch-size",
		0,
		"lote imediato explícito de até 50 sessões, sem cadência horária",
	)
	pausedOK := flags.Bool(
		"paused-ok",
		false,
		"registra canário não promovido como pausa observável",
	)
	autoPREnabled := flags.Bool(
		"auto-pr-enabled",
		false,
		"kill switch explícito para criação automática de PR",
	)
	openPRsPath := flags.String(
		"open-prs",
		"",
		"inventário vivo de PRs abertos contra jules/inbox",
	)
	inboxSHA := flags.String(
		"inbox-sha",
		"",
		"SHA remoto observado de jules/inbox",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *confirmTarget != proDailySessionLimit {
		return fmt.Errorf(
			"--confirm-target deve ser exatamente %d",
			proDailySessionLimit,
		)
	}
	if *immediateBatchSize < 0 || *immediateBatchSize > maxImmediateBatchSize {
		return fmt.Errorf(
			"--immediate-batch-size deve estar entre 0 e %d",
			maxImmediateBatchSize,
		)
	}
	immediate := *immediateBatchSize > 0
	if immediate && (*promoted || *retryCanary) {
		return errors.New(
			"--immediate-batch-size não aceita --promoted ou --retry-canary",
		)
	}
	if *manifestPath == "" || *output == "" {
		return errors.New("--manifest e --output são obrigatórios")
	}
	if !*autoPREnabled {
		return errors.New("auto-PR desabilitado; use --auto-pr-enabled após autorização")
	}
	if *openPRsPath == "" {
		return errors.New("--open-prs é obrigatório com auto-PR habilitado")
	}
	openPRCount, err := loadJulesInboxOpenPRCount(*openPRsPath)
	if err != nil {
		return err
	}
	slots, err := hourlySlots(*hour)
	if err != nil {
		return err
	}
	manifest, err := loadDailyManifest(*manifestPath)
	if err != nil {
		return err
	}
	if immediate && !manifest.ImplementationOnly {
		return errors.New(
			"lote imediato exige manifesto --implementation-only",
		)
	}
	if err := validateDailyInboxBase(manifest, *inboxSHA); err != nil {
		return err
	}
	sessions, err := listAllSessions(ctx, client)
	if err != nil {
		return err
	}
	cutoff := now.Add(-dailyQuotaWindow)
	recent := 0
	active := 0
	dailyActive := 0
	dailyRecent := 0
	dailyNonTerminal := 0
	existingTitles := make(map[string]struct{}, len(sessions))
	existingRevisions := make(
		map[dailyIssueRevision]struct{},
		len(sessions),
	)
	for _, session := range sessions {
		existingTitles[session.Title] = struct{}{}
		if strings.HasPrefix(session.Title, dailyTitlePrefix) {
			if revision, revisionErr := issueRevisionFromSession(session); revisionErr == nil {
				existingRevisions[revision] = struct{}{}
			}
		}
		if isConcurrentJulesState(session.State) {
			active++
			if strings.HasPrefix(session.Title, dailyTitlePrefix) {
				dailyActive++
			}
		}
		if strings.HasPrefix(session.Title, dailyTitlePrefix) &&
			!isTerminalJulesState(session.State) {
			dailyNonTerminal++
		}
		if session.CreateTime.IsZero() {
			return fmt.Errorf(
				"sessão %s sem createTime; quota não observável",
				session.ID,
			)
		}
		if session.CreateTime.After(now.Add(5 * time.Minute)) {
			return fmt.Errorf(
				"sessão %s possui createTime futuro; quota não observável",
				session.ID,
			)
		}
		if session.CreateTime.Before(cutoff) {
			continue
		}
		recent++
		if strings.HasPrefix(session.Title, dailyTitlePrefix) {
			dailyRecent++
		}
	}
	canary := dailyRecent == 0
	if !immediate && *retryCanary {
		if *promoted {
			return errors.New(
				"--retry-canary e --promoted são mutuamente exclusivos",
			)
		}
		if canary {
			return errors.New(
				"--retry-canary exige um canário anterior na janela",
			)
		}
		if dailyNonTerminal != 0 {
			return fmt.Errorf(
				"retry exige canário anterior terminal; %d sessão(ões) pendente(s)",
				dailyNonTerminal,
			)
		}
		if dailyRecent != dailyCanarySize {
			return fmt.Errorf(
				"retry permite 1 tentativa após exatamente %d sessões; observadas %d",
				dailyCanarySize,
				dailyRecent,
			)
		}
	}
	if !immediate && !canary && !*promoted && !*retryCanary {
		if *pausedOK {
			return writePrivateJSON(*output, dailyDispatchArtifact{
				GeneratedAt:   now.UTC().Format(time.RFC3339),
				Hour:          *hour,
				Requested:     0,
				Recent24Hours: recent,
				ActiveBefore:  active,
				OpenPRsBefore: openPRCount,
				ReservedPRs:   dailyActive,
				Canary:        true,
				CanaryAttempt: 1,
				PromptVersion: dailyPromptVersion,
			})
		}
		return errors.New("canário existente ainda não foi promovido pelo operador")
	}
	openPRSlots, err := remainingJulesInboxPRSlots(
		openPRCount,
		dailyActive,
	)
	if err != nil {
		return err
	}
	requested := slots
	if immediate {
		requested = *immediateBatchSize
	}
	canaryWindow := !immediate && (canary || *retryCanary)
	if canaryWindow {
		requested = minInt(requested, dailyCanarySize)
	}
	requested = minInt(requested, maxInt(0, proDailySessionLimit-recent))
	requested = minInt(
		requested,
		maxInt(0, proConcurrentSessionLimit-active),
	)
	requested = minInt(requested, openPRSlots)
	candidates := make([]manifestSession, 0, requested)
	for _, item := range manifest.Sessions {
		if len(candidates) == requested {
			break
		}
		if canaryWindow && !item.CanaryEligible {
			continue
		}
		candidate := item
		if *retryCanary {
			candidate.Title = strings.Replace(
				item.Title,
				dailyTitlePrefix,
				"[RS-EXAMPLE R2 ",
				1,
			)
			candidate.Prompt += "\n\nTENTATIVA_CANARIO: R2 — retry único da mesma revisão após falha terminal; recomece da main e não reutilize afirmações ou arquivos locais da tentativa anterior."
			if !validTitle(candidate.Title) ||
				!validPrompt(manifest.CommonPrompt+"\n\n"+candidate.Prompt) {
				return fmt.Errorf(
					"candidata de retry #%d excede os limites de título/prompt",
					candidate.IssueNumber,
				)
			}
		}
		if _, exists := existingTitles[candidate.Title]; exists {
			continue
		}
		revision := dailyIssueRevision{
			Number:    candidate.IssueNumber,
			UpdatedAt: candidate.IssueUpdatedAt,
		}
		if _, exists := existingRevisions[revision]; exists {
			continue
		}
		candidates = append(candidates, candidate)
		if len(candidates) == requested {
			break
		}
	}
	if len(candidates) != requested {
		return fmt.Errorf(
			"janela exige %d candidato(s), mas somente %d são seguros e inéditos",
			requested,
			len(candidates),
		)
	}
	canaryAttempt := 0
	if canaryWindow && canary {
		canaryAttempt = 1
	}
	if *retryCanary {
		canaryAttempt = 2
	}
	artifact := dailyDispatchArtifact{
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		Hour:               *hour,
		Requested:          requested,
		Created:            make([]sessionRecord, 0, requested),
		Recent24Hours:      recent,
		ActiveBefore:       active,
		OpenPRsBefore:      openPRCount,
		ReservedPRs:        dailyActive,
		Canary:             canaryWindow,
		CanaryAttempt:      canaryAttempt,
		ImmediateBatchSize: *immediateBatchSize,
		PromptVersion:      dailyPromptVersion,
	}
	for _, item := range candidates {
		body := map[string]any{
			"title":               item.Title,
			"prompt":              strings.TrimSpace(manifest.CommonPrompt) + "\n\n" + strings.TrimSpace(item.Prompt),
			"requirePlanApproval": manifest.RequirePlanApproval,
			"automationMode":      manifest.AutomationMode,
			"sourceContext": map[string]any{
				"source": manifest.Source,
				"githubRepoContext": map[string]any{
					"startingBranch": manifest.StartingBranch,
				},
			},
		}
		raw, requestErr := client.do(ctx, http.MethodPost, sessionsEndpoint, body)
		if requestErr != nil {
			if immediate && isJulesHTTPStatus(requestErr, http.StatusBadRequest) {
				artifact.UpstreamCapacityReached = true
				return writePrivateJSON(*output, artifact)
			}
			_ = writePrivateJSON(*output, artifact)
			return fmt.Errorf(
				"dispatch interrompido após %d criação(ões): %w",
				len(artifact.Created),
				requestErr,
			)
		}
		record, parseErr := parseSession(raw)
		if parseErr != nil {
			return parseErr
		}
		artifact.Created = append(artifact.Created, record)
	}
	return writePrivateJSON(*output, artifact)
}

type julesInboxOpenPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	State       string `json:"state"`
}

func loadJulesInboxOpenPRCount(path string) (int, error) {
	var pullRequests []julesInboxOpenPR
	if err := readJSONFile(path, &pullRequests); err != nil {
		return 0, fmt.Errorf("ler inventário de PRs de intake: %w", err)
	}
	seen := make(map[int]struct{}, len(pullRequests))
	for _, pullRequest := range pullRequests {
		if pullRequest.Number <= 0 ||
			pullRequest.URL != "https://github.com/emersonbusson/ramshared/pull/"+
				strconv.Itoa(pullRequest.Number) ||
			pullRequest.BaseRefName != julesInboxBranch ||
			pullRequest.State != "OPEN" {
			return 0, errors.New("inventário contém PR fora da fronteira de intake")
		}
		if _, duplicate := seen[pullRequest.Number]; duplicate {
			return 0, errors.New("inventário contém PR duplicado")
		}
		seen[pullRequest.Number] = struct{}{}
	}
	return len(pullRequests), nil
}

func remainingJulesInboxPRSlots(open, reserved int) (int, error) {
	total := open + reserved
	if open < 0 || reserved < 0 || total > maxOpenJulesInboxPRs {
		return 0, fmt.Errorf(
			"backlog de PRs fora do limite observável: %d abertos + %d reservados / %d",
			open,
			reserved,
			maxOpenJulesInboxPRs,
		)
	}
	return maxOpenJulesInboxPRs - total, nil
}

func loadDailyManifest(path string) (sessionManifest, error) {
	var manifest sessionManifest
	if err := readJSONFile(path, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Source != allowedSourceName ||
		manifest.StartingBranch != julesInboxBranch {
		return manifest, errors.New(
			"manifesto diário deve apontar para emersonbusson/ramshared:jules/inbox",
		)
	}
	if !isFullGitSHA(manifest.BaseSHA) {
		return manifest, errors.New("manifesto diário possui baseSha inválido")
	}
	if manifest.RequirePlanApproval ||
		manifest.AutomationMode != autoCreatePRAutomation {
		return manifest, errors.New(
			"manifesto diário exige autoaprovação e AUTO_CREATE_PR",
		)
	}
	if len(manifest.Sessions) != proDailySessionLimit {
		return manifest, fmt.Errorf(
			"manifesto diário deve conter exatamente %d sessões",
			proDailySessionLimit,
		)
	}
	for index, item := range manifest.Sessions {
		prompt := manifest.CommonPrompt + "\n\n" + item.Prompt
		if !validTitle(item.Title) || !validPrompt(prompt) {
			return manifest, fmt.Errorf(
				"sessão diária %d possui título/prompt inválido",
				index+1,
			)
		}
		if !manifest.ImplementationOnly &&
			index < dailyCanarySize &&
			!item.CanaryEligible {
			return manifest, fmt.Errorf(
				"sessão diária %d não é elegível para o canário",
				index+1,
			)
		}
		if item.IssueNumber <= 0 || item.IssueUpdatedAt == "" {
			return manifest, fmt.Errorf(
				"sessão diária %d não contém revisão do issue",
				index+1,
			)
		}
		if _, err := time.Parse(time.RFC3339, item.IssueUpdatedAt); err != nil {
			return manifest, fmt.Errorf(
				"sessão diária %d possui issueUpdatedAt inválido",
				index+1,
			)
		}
	}
	return manifest, nil
}

func validateDailyInboxBase(manifest sessionManifest, inboxSHA string) error {
	if !isFullGitSHA(inboxSHA) {
		return errors.New("SHA observado de jules/inbox é inválido")
	}
	if inboxSHA != manifest.BaseSHA {
		return fmt.Errorf(
			"jules/inbox está defasada: inbox=%s main=%s",
			inboxSHA,
			manifest.BaseSHA,
		)
	}
	return nil
}

func isConcurrentJulesState(state string) bool {
	switch state {
	case julesStateAwaitingPlan,
		julesStateAwaitingUser,
		julesStatePaused,
		julesStateCompleted,
		julesStateFailed:
		return false
	default:
		return true
	}
}

func isTerminalJulesState(state string) bool {
	return state == julesStateCompleted || state == julesStateFailed
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
