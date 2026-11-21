package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxSessionPages               = 100
	maxActivityPages              = 500
	activityPageSize              = 10
	planActivitySignalFields      = "nextPageToken,activities(planGenerated.plan.steps,planApproved)"
	approvalActivitySignalFields  = "nextPageToken,activities(planGenerated.plan.steps,planApproved,artifacts.changeSet)"
	reconcileActivitySignalFields = "nextPageToken,activities(createTime,agentMessaged.agentMessage,userMessaged.userMessage)"
)

type sessionManifest struct {
	Source              string            `json:"source"`
	StartingBranch      string            `json:"startingBranch"`
	BaseSHA             string            `json:"baseSha,omitempty"`
	ImplementationOnly  bool              `json:"implementationOnly,omitempty"`
	RequirePlanApproval bool              `json:"requirePlanApproval"`
	AutomationMode      string            `json:"automationMode"`
	CommonPrompt        string            `json:"commonPrompt"`
	Sessions            []manifestSession `json:"sessions"`
}

type manifestSession struct {
	Title          string `json:"title"`
	Prompt         string `json:"prompt"`
	CanaryEligible bool   `json:"canaryEligible"`
	IssueNumber    int    `json:"issueNumber,omitempty"`
	IssueUpdatedAt string `json:"issueUpdatedAt,omitempty"`
}

type sessionRecord struct {
	ID         string          `json:"id"`
	Title      string          `json:"title,omitempty"`
	State      string          `json:"state,omitempty"`
	CreateTime time.Time       `json:"createTime,omitempty"`
	UpdateTime time.Time       `json:"updateTime,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

type batchArtifact struct {
	GeneratedAt string          `json:"generatedAt"`
	Source      string          `json:"source,omitempty"`
	Sessions    []sessionRecord `json:"sessions"`
}

func runBatch(ctx context.Context, client *julesClient, args []string) error {
	if len(args) == 0 {
		return errors.New(
			"uso: batch inventory|launch|status|measure|cleanup|" +
				"daily-issue-inventory|daily-plan|daily-dispatch|" +
				"daily-reconcile|daily-metrics|daily-reap|daily-finalize|" +
				"daily-pr-audit-validate|" +
				"daily-pr-audit-live",
		)
	}
	switch args[0] {
	case "inventory":
		return runInventory(ctx, client, args[1:])
	case "launch":
		return runLaunch(ctx, client, args[1:])
	case "status":
		return runStatus(ctx, client, args[1:])
	case "measure":
		return runMeasure(ctx, client, args[1:])
	case "cleanup":
		return runCleanup(ctx, client, args[1:])
	case "daily-issue-inventory":
		return runDailyIssueInventory(args[1:])
	case "daily-plan":
		return runDailyPlan(args[1:])
	case "daily-dispatch":
		return runDailyDispatch(ctx, client, args[1:], time.Now())
	case "daily-reconcile":
		return runDailyReconcile(ctx, client, args[1:], time.Now())
	case "daily-metrics":
		return runDailyMetrics(ctx, client, args[1:])
	case "daily-reap":
		return runDailyReap(ctx, client, args[1:])
	case "daily-finalize":
		return runDailyFinalize(ctx, client, args[1:])
	case "daily-pr-audit-validate":
		return runDailyPRAuditValidate(args[1:])
	case "daily-pr-audit-live":
		return runDailyPRAuditLive(args[1:])
	default:
		return fmt.Errorf("comando batch desconhecido: %s", args[0])
	}
}

type planMetric struct {
	SessionID         string          `json:"sessionId"`
	Title             string          `json:"title"`
	State             string          `json:"state"`
	PlanCount         int             `json:"planCount"`
	PlanApprovalCount int             `json:"planApprovalCount"`
	ChangeSetCount    int             `json:"changeSetCount"`
	Score             int             `json:"score"`
	Checks            map[string]bool `json:"checks"`
	Missing           []string        `json:"missing"`
	HardGates         []string        `json:"hardGates"`
	EligibleForReview bool            `json:"eligibleForReview"`
}

type generatedPlanStep struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func runMeasure(ctx context.Context, client *julesClient, args []string) error {
	flags := flag.NewFlagSet("measure", flag.ContinueOnError)
	launchPath := flags.String("launch", "", "artefato de launch")
	output := flags.String("output", "", "arquivo JSON de saída")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *launchPath == "" || *output == "" {
		return errors.New("--launch e --output são obrigatórios")
	}
	artifact, err := loadArtifact(*launchPath)
	if err != nil {
		return err
	}
	metrics := make([]planMetric, 0, len(artifact.Sessions))
	for _, item := range artifact.Sessions {
		sessionRaw, requestErr := client.do(ctx, http.MethodGet, "/sessions/"+item.ID, nil)
		if requestErr != nil {
			return requestErr
		}
		session, parseErr := parseSession(sessionRaw)
		if parseErr != nil {
			return parseErr
		}
		activitiesRaw, requestErr := listAllApprovalSignals(ctx, client, item.ID)
		if requestErr != nil {
			return requestErr
		}
		metric, parseErr := measureLatestPlan(session, activitiesRaw)
		if parseErr != nil {
			return parseErr
		}
		metrics = append(metrics, metric)
	}
	return writePrivateJSON(*output, map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"metrics":     metrics,
	})
}

func listAllActivities(ctx context.Context, client *julesClient, sessionID string) (json.RawMessage, error) {
	return listAllActivitiesWithFields(ctx, client, sessionID, "")
}

func listAllPlanSignals(ctx context.Context, client *julesClient, sessionID string) (json.RawMessage, error) {
	return listAllActivitiesWithFields(ctx, client, sessionID, planActivitySignalFields)
}

func listAllApprovalSignals(ctx context.Context, client *julesClient, sessionID string) (json.RawMessage, error) {
	return listAllActivitiesWithFields(ctx, client, sessionID, approvalActivitySignalFields)
}

func listAllReconcileSignals(ctx context.Context, client *julesClient, sessionID string) (json.RawMessage, error) {
	return listAllActivitiesWithFields(ctx, client, sessionID, reconcileActivitySignalFields)
}

func listAllActivitiesWithFields(
	ctx context.Context,
	client *julesClient,
	sessionID string,
	fields string,
) (json.RawMessage, error) {
	activities, err := listAllActivitiesWithPageSize(
		ctx,
		client,
		sessionID,
		activityPageSize,
		fields,
	)
	if err != nil && errors.Is(err, errJulesResponseTooLarge) {
		return listAllActivitiesWithPageSize(ctx, client, sessionID, 1, fields)
	}
	return activities, err
}

func listAllActivitiesWithPageSize(
	ctx context.Context,
	client *julesClient,
	sessionID string,
	pageSize int,
	fields string,
) (json.RawMessage, error) {
	activities := make([]json.RawMessage, 0)
	token := ""
	seenTokens := map[string]struct{}{}
	for range maxActivityPages {
		path := fmt.Sprintf(
			"/sessions/%s/activities?pageSize=%d",
			sessionID,
			pageSize,
		)
		if fields != "" {
			path += "&fields=" + urlQueryEscape(fields)
		}
		if token != "" {
			path += "&pageToken=" + urlQueryEscape(token)
		}
		responseLimit := maxResponseBytes
		if pageSize == 1 {
			responseLimit = maxSingleActivityBytes
		}
		raw, err := client.doWithResponseLimit(
			ctx,
			http.MethodGet,
			path,
			nil,
			responseLimit,
		)
		if err != nil {
			return nil, err
		}
		var page struct {
			Activities    []json.RawMessage `json:"activities"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("interpretar página de atividades: %w", err)
		}
		activities = append(activities, page.Activities...)
		if page.NextPageToken == "" {
			return marshalActivities(activities)
		}
		if _, exists := seenTokens[page.NextPageToken]; exists {
			return nil, errors.New("paginação de atividades repetiu pageToken")
		}
		seenTokens[page.NextPageToken] = struct{}{}
		token = page.NextPageToken
	}
	return nil, fmt.Errorf(
		"paginação de atividades excedeu limite de páginas: %d",
		maxActivityPages,
	)
}

func marshalActivities(activities []json.RawMessage) (json.RawMessage, error) {
	combined, err := json.Marshal(map[string]any{"activities": activities})
	if err != nil {
		return nil, fmt.Errorf("agregar atividades: %w", err)
	}
	return combined, nil
}

func measureLatestPlan(session sessionRecord, activitiesRaw json.RawMessage) (planMetric, error) {
	return measureLatestPlanWithPolicy(session, activitiesRaw, false)
}

func measureLatestAutoPRPlan(
	session sessionRecord,
	activitiesRaw json.RawMessage,
) (planMetric, error) {
	return measureLatestPlanWithPolicy(session, activitiesRaw, true)
}

func measureLatestPlanWithPolicy(
	session sessionRecord,
	activitiesRaw json.RawMessage,
	autoPR bool,
) (planMetric, error) {
	var response struct {
		Activities []struct {
			PlanGenerated *struct {
				Plan struct {
					Steps []generatedPlanStep `json:"steps"`
				} `json:"plan"`
			} `json:"planGenerated"`
			PlanApproved *json.RawMessage `json:"planApproved"`
			Artifacts    []struct {
				ChangeSet *json.RawMessage `json:"changeSet"`
			} `json:"artifacts"`
		} `json:"activities"`
	}
	if err := json.Unmarshal(activitiesRaw, &response); err != nil {
		return planMetric{}, fmt.Errorf("interpretar atividades: %w", err)
	}
	var planText string
	var latestPlanSteps []generatedPlanStep
	planCount := 0
	approvalCount := 0
	changeSetCount := 0
	for _, activity := range response.Activities {
		if activity.PlanApproved != nil {
			approvalCount++
		}
		for _, artifact := range activity.Artifacts {
			if artifact.ChangeSet != nil {
				changeSetCount++
			}
		}
		if activity.PlanGenerated == nil {
			continue
		}
		planCount++
		latestPlanSteps = activity.PlanGenerated.Plan.Steps
		var parts []string
		for _, step := range activity.PlanGenerated.Plan.Steps {
			parts = append(parts, step.Title, step.Description)
		}
		planText = strings.ToLower(strings.Join(parts, " "))
	}
	checks := map[string]bool{
		"rules": containsAny(planText, "agents.md", ".claude/rules", "claude.md"),
		"mainDiff": containsAny(planText, "origin/main", "main..") ||
			(strings.Contains(planText, "main_diff:") &&
				containsFullGitSHA(planText)),
		"files":          containsAny(planText, "arquivo", "file"),
		"invariants":     strings.Contains(planText, "invariant"),
		"counterfactual": strings.Contains(planText, "counterfactual"),
		"redTest":        strings.Contains(planText, "red"),
		"coverage":       containsAny(planText, "80%", "cobertura", "coverage"),
		"realProof":      containsAny(planText, "postgresql", "login real", "seed", "screenshot", "idempot"),
		"rollback":       strings.Contains(planText, "rollback"),
	}
	if autoPR {
		checks["prBoundary"] =
			strings.Contains(planText, julesInboxBranch) &&
				containsAny(planText, "não faça merge", "não fará merge", "sem merge") &&
				containsAny(planText, "consolidação", "consolidado", "reimplementação")
	} else {
		checks["noAutoPR"] = containsAny(
			planText,
			"no_auto_pr",
			"sem criar pull request",
			"sem pr",
			"não abra pr",
			"não abrir pr",
			"no pr",
		)
	}
	var missing []string
	score := 0
	for name, passed := range checks {
		if passed {
			score++
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	var hardGates []string
	if autoPR {
		hardGates = append(
			hardGates,
			validateAutoPRPlanStructure(latestPlanSteps)...,
		)
		if changeSetCount > 0 && planCount == 0 {
			hardGates = append(hardGates, "changeSet sem plano observável")
		}
	} else if changeSetCount > 0 && approvalCount == 0 {
		hardGates = append(hardGates, "changeSet sem planApproved")
	}
	return planMetric{
		SessionID: session.ID, Title: session.Title, State: session.State,
		PlanCount: planCount, PlanApprovalCount: approvalCount,
		ChangeSetCount: changeSetCount, Score: score, Checks: checks,
		Missing: missing, HardGates: hardGates,
		EligibleForReview: score == 10 && len(hardGates) == 0,
	}, nil
}

func validateAutoPRPlanStructure(steps []generatedPlanStep) []string {
	prefixes := []string{
		"RULES:",
		"MAIN_DIFF:",
		"FILES:",
		"INVARIANTS:",
		"COUNTERFACTUAL:",
		"RED_TEST:",
		"COVERAGE:",
		"REAL_PROOF:",
		"ROLLBACK:",
		"PR_BOUNDARY:",
	}
	normalized := make([]generatedPlanStep, 0, len(steps))
	preCommitSeen := false
	for _, step := range steps {
		title := strings.TrimSpace(step.Title)
		preCommitPrefix := ""
		if strings.HasPrefix(title, "PRE-COMMIT:") {
			preCommitPrefix = "PRE-COMMIT:"
		} else if strings.HasPrefix(title, "PRE_COMMIT:") {
			preCommitPrefix = "PRE_COMMIT:"
		}
		if preCommitPrefix != "" {
			if preCommitSeen {
				return []string{"plano RS-ARCHIVE possui mais de 1 passo PRE-COMMIT"}
			}
			if !planStepHasPayload(step, preCommitPrefix) {
				return []string{"passo PRE-COMMIT não possui conteúdo"}
			}
			preCommitSeen = true
			continue
		}
		normalized = append(normalized, step)
	}
	if len(normalized) != len(prefixes) {
		return []string{fmt.Sprintf(
			"plano RS-ARCHIVE exige exatamente %d blocos contratuais; observados %d",
			len(prefixes),
			len(normalized),
		)}
	}
	for index, prefix := range prefixes {
		title := strings.TrimSpace(normalized[index].Title)
		if !strings.HasPrefix(title, prefix) {
			return []string{fmt.Sprintf(
				"passo RS-ARCHIVE %d deve iniciar com %s",
				index+1,
				prefix,
			)}
		}
		if !planStepHasPayload(normalized[index], prefix) {
			return []string{fmt.Sprintf(
				"passo RS-ARCHIVE %d não possui conteúdo após %s",
				index+1,
				prefix,
			)}
		}
	}
	return nil
}

func planStepHasPayload(step generatedPlanStep, prefix string) bool {
	titlePayload := strings.TrimSpace(
		strings.TrimPrefix(strings.TrimSpace(step.Title), prefix),
	)
	return titlePayload != "" || strings.TrimSpace(step.Description) != ""
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func containsFullGitSHA(value string) bool {
	parts := strings.FieldsFunc(value, func(character rune) bool {
		return !strings.ContainsRune("0123456789abcdef", character)
	})
	for _, part := range parts {
		if isFullGitSHA(part) {
			return true
		}
	}
	return false
}

func runInventory(ctx context.Context, client *julesClient, args []string) error {
	flags := flag.NewFlagSet("inventory", flag.ContinueOnError)
	output := flags.String("output", "", "arquivo JSON de saída")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output é obrigatório")
	}
	sessions, err := listAllSessions(ctx, client)
	if err != nil {
		return err
	}
	return writeArtifact(*output, batchArtifact{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Sessions: sessions})
}

func runLaunch(ctx context.Context, client *julesClient, args []string) error {
	flags := flag.NewFlagSet("launch", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "", "manifesto JSON")
	output := flags.String("output", "", "arquivo JSON de saída")
	confirmCount := flags.Int("confirm-count", -1, "quantidade exata autorizada")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *output == "" {
		return errors.New("--manifest e --output são obrigatórios")
	}
	manifest, err := loadManifest(*manifestPath)
	if err != nil {
		return err
	}
	if *confirmCount != len(manifest.Sessions) {
		return fmt.Errorf("--confirm-count deve ser exatamente %d", len(manifest.Sessions))
	}

	created := make([]sessionRecord, 0, len(manifest.Sessions))
	for _, item := range manifest.Sessions {
		prompt := strings.TrimSpace(manifest.CommonPrompt) + "\n\n" + strings.TrimSpace(item.Prompt)
		body := map[string]any{
			"title":               item.Title,
			"prompt":              prompt,
			"requirePlanApproval": true,
			"sourceContext": map[string]any{
				"source":            manifest.Source,
				"githubRepoContext": map[string]any{"startingBranch": manifest.StartingBranch},
			},
		}
		raw, requestErr := client.do(ctx, http.MethodPost, sessionsEndpoint, body)
		if requestErr != nil {
			// O artefato parcial impede repetir silenciosamente sessões já criadas.
			_ = writeArtifact(*output, batchArtifact{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Source: manifest.Source, Sessions: created})
			return fmt.Errorf("criar sessão %q após %d sucesso(s): %w", item.Title, len(created), requestErr)
		}
		record, parseErr := parseSession(raw)
		if parseErr != nil {
			return fmt.Errorf("interpretar sessão %q: %w", item.Title, parseErr)
		}
		created = append(created, record)
	}
	return writeArtifact(*output, batchArtifact{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Source: manifest.Source, Sessions: created})
}

func runStatus(ctx context.Context, client *julesClient, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	launchPath := flags.String("launch", "", "artefato de launch ou inventory")
	output := flags.String("output", "", "arquivo JSON de saída")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *launchPath == "" || *output == "" {
		return errors.New("--launch e --output são obrigatórios")
	}
	artifact, err := loadArtifact(*launchPath)
	if err != nil {
		return err
	}
	current := make([]sessionRecord, 0, len(artifact.Sessions))
	for _, item := range artifact.Sessions {
		if !sessionIDPattern.MatchString(item.ID) {
			return fmt.Errorf("sessionId inválido no artefato: %q", item.ID)
		}
		raw, requestErr := client.do(ctx, http.MethodGet, "/sessions/"+item.ID, nil)
		if requestErr != nil {
			return fmt.Errorf("consultar sessão %s: %w", item.ID, requestErr)
		}
		record, parseErr := parseSession(raw)
		if parseErr != nil {
			return parseErr
		}
		current = append(current, record)
	}
	return writeArtifact(*output, batchArtifact{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Source: artifact.Source, Sessions: current})
}

func runCleanup(ctx context.Context, client *julesClient, args []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	inventoryPath := flags.String("inventory", "", "inventário previamente salvo")
	confirmCount := flags.Int("confirm-count", -1, "quantidade exata autorizada")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *inventoryPath == "" {
		return errors.New("--inventory é obrigatório")
	}
	artifact, err := loadArtifact(*inventoryPath)
	if err != nil {
		return err
	}
	if *confirmCount != len(artifact.Sessions) {
		return fmt.Errorf("--confirm-count deve ser exatamente %d", len(artifact.Sessions))
	}
	for index, item := range artifact.Sessions {
		if !sessionIDPattern.MatchString(item.ID) {
			return fmt.Errorf("sessionId inválido no item %d", index+1)
		}
		if _, requestErr := client.do(ctx, http.MethodDelete, "/sessions/"+item.ID, nil); requestErr != nil {
			return fmt.Errorf("excluir sessão %s após %d exclusão(ões): %w", item.ID, index, requestErr)
		}
	}
	remaining, err := listAllSessions(ctx, client)
	if err != nil {
		return fmt.Errorf("verificar limpeza: %w", err)
	}
	selected := make(map[string]struct{}, len(artifact.Sessions))
	for _, item := range artifact.Sessions {
		selected[item.ID] = struct{}{}
	}
	for _, item := range remaining {
		if _, exists := selected[item.ID]; exists {
			return fmt.Errorf("limpeza incompleta: sessão %s ainda existe", item.ID)
		}
	}
	return nil
}

func loadManifest(path string) (sessionManifest, error) {
	var manifest sessionManifest
	if err := readJSONFile(path, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Source != allowedSourceName {
		return manifest, errors.New("manifesto deve usar sources/github/emersonbusson/ramshared")
	}
	if !manifest.RequirePlanApproval {
		return manifest, errors.New("requirePlanApproval deve ser true")
	}
	if manifest.AutomationMode != "" && manifest.AutomationMode != unspecifiedAutomation {
		return manifest, errors.New("automationMode automático não é permitido")
	}
	if !regexpBranch.MatchString(manifest.StartingBranch) {
		return manifest, errors.New("startingBranch inválida")
	}
	maxBatch := 3
	if configured := os.Getenv("JULES_MAX_BATCH_SESSIONS"); configured != "" {
		parsed, err := strconv.Atoi(configured)
		if err != nil || parsed < 1 || parsed > 60 {
			return manifest, errors.New("JULES_MAX_BATCH_SESSIONS deve estar entre 1 e 60")
		}
		maxBatch = parsed
	}
	if len(manifest.Sessions) == 0 || len(manifest.Sessions) > maxBatch {
		return manifest, fmt.Errorf("manifesto deve conter de 1 a %d sessões", maxBatch)
	}
	for index, item := range manifest.Sessions {
		prompt := strings.TrimSpace(manifest.CommonPrompt) + "\n\n" + strings.TrimSpace(item.Prompt)
		if !validTitle(item.Title) || !validPrompt(prompt) {
			return manifest, fmt.Errorf("sessão %d possui título/prompt inválido", index+1)
		}
	}
	return manifest, nil
}

var regexpBranch branchMatcher

type branchMatcher struct{}

func (branchMatcher) MatchString(value string) bool {
	if value == "" || len(value) > 120 || strings.HasPrefix(value, "/") || strings.Contains(value, "..") {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/-", character) {
			return false
		}
	}
	return true
}

func listAllSessions(ctx context.Context, client *julesClient) ([]sessionRecord, error) {
	const fields = "nextPageToken,sessions(id,name,prompt,title,state,createTime,updateTime,outputs.pullRequest.url)"
	sessions := make([]sessionRecord, 0)
	token := ""
	seenTokens := map[string]struct{}{}
	for range maxSessionPages {
		path := sessionsEndpoint + "?pageSize=100&fields=" + urlQueryEscape(fields)
		if token != "" {
			path += "&pageToken=" + urlQueryEscape(token)
		}
		raw, err := client.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Sessions      []json.RawMessage `json:"sessions"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("interpretar página de sessões: %w", err)
		}
		for _, rawSession := range page.Sessions {
			record, parseErr := parseSession(rawSession)
			if parseErr != nil {
				return nil, parseErr
			}
			sessions = append(sessions, record)
		}
		if page.NextPageToken == "" {
			return sessions, nil
		}
		if _, exists := seenTokens[page.NextPageToken]; exists {
			return nil, errors.New("paginação de sessões repetiu pageToken")
		}
		seenTokens[page.NextPageToken] = struct{}{}
		token = page.NextPageToken
	}
	return nil, fmt.Errorf(
		"paginação de sessões excedeu limite de páginas: %d",
		maxSessionPages,
	)
}

func parseSession(raw json.RawMessage) (sessionRecord, error) {
	var fields struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Title      string `json:"title"`
		State      string `json:"state"`
		CreateTime string `json:"createTime"`
		UpdateTime string `json:"updateTime"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return sessionRecord{}, fmt.Errorf("interpretar sessão: %w", err)
	}
	id := fields.ID
	if id == "" {
		id = strings.TrimPrefix(fields.Name, "sessions/")
	}
	if !sessionIDPattern.MatchString(id) {
		return sessionRecord{}, errors.New("resposta Jules sem sessionId válido")
	}
	var createTime, updateTime time.Time
	if fields.CreateTime != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, fields.CreateTime)
		if parseErr != nil {
			return sessionRecord{}, errors.New(
				"resposta Jules com createTime inválido",
			)
		}
		createTime = parsed
	}
	if fields.UpdateTime != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, fields.UpdateTime)
		if parseErr != nil {
			return sessionRecord{}, errors.New(
				"resposta Jules com updateTime inválido",
			)
		}
		updateTime = parsed
	}
	return sessionRecord{
		ID:         id,
		Title:      fields.Title,
		State:      fields.State,
		CreateTime: createTime,
		UpdateTime: updateTime,
		Raw:        raw,
	}, nil
}

func loadArtifact(path string) (batchArtifact, error) {
	var artifact batchArtifact
	if err := readJSONFile(path, &artifact); err != nil {
		return artifact, err
	}
	if len(artifact.Sessions) == 0 {
		return artifact, errors.New("artefato não contém sessões")
	}
	return artifact, nil
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("ler %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("interpretar %s: %w", path, err)
	}
	return nil
}

func writeArtifact(path string, artifact batchArtifact) error {
	return writePrivateJSON(path, artifact)
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("criar diretório de artefato: %w", err)
	}
	temp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("gravar artefato temporário: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("publicar artefato: %w", err)
	}
	return nil
}

func urlQueryEscape(value string) string {
	// Tokens de paginação são opacos e nunca são concatenados sem escape.
	return url.QueryEscape(value)
}
