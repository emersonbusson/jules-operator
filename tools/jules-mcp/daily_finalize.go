package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dailyFinalizationMarkerVersion = "v1"
	// finalizationSessionFields excludes activities and change sets: finalization
	// only needs immutable identity, prompt revision and PR URL. The projection
	// keeps the API response within the existing 8 MiB fail-closed cap.
	finalizationSessionFields = "id,name,prompt,title,outputs.pullRequest.url"
)

type dailyFinalizationManifest struct {
	ConsolidatedPR int      `json:"consolidatedPr"`
	AuditPaths     []string `json:"auditPaths"`
}

type dailyFinalizationCheck struct {
	Name         string `json:"name"`
	Context      string `json:"context"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	State        string `json:"state"`
	Conclusion   string `json:"conclusion"`
	StartedAt    string `json:"startedAt"`
}

type dailyFinalizationConsolidatedPR struct {
	Number            int                      `json:"number"`
	URL               string                   `json:"url"`
	BaseRefName       string                   `json:"baseRefName"`
	HeadRefOID        string                   `json:"headRefOid"`
	State             string                   `json:"state"`
	MergedAt          string                   `json:"mergedAt"`
	StatusCheckRollup []dailyFinalizationCheck `json:"statusCheckRollup"`
}

type dailyFinalizationSourcePR struct {
	Number   int    `json:"number"`
	URL      string `json:"url"`
	State    string `json:"state"`
	MergedAt string `json:"mergedAt"`
}

type dailyFinalizationReceipt struct {
	Version             string `json:"version"`
	FinalizedAt         string `json:"finalizedAt"`
	SessionID           string `json:"sessionId"`
	SourcePR            int    `json:"sourcePr"`
	SourcePRURL         string `json:"sourcePrUrl"`
	Disposition         string `json:"disposition"`
	ConsolidatedPR      int    `json:"consolidatedPr"`
	ConsolidatedPRURL   string `json:"consolidatedPrUrl"`
	ConsolidatedHeadSHA string `json:"consolidatedHeadSha"`
	CommentMarker       string `json:"commentMarker"`
	PRClosedVerified    bool   `json:"prClosedVerified"`
	IssueNumber         int    `json:"issueNumber,omitempty"`
	IssueUpdatedAt      string `json:"issueUpdatedAt,omitempty"`
}

type dailyFinalizationAuditInput struct {
	Path     string
	Audit    dailyPRAudit
	Revision dailyIssueRevision
}

func runDailyFinalize(
	ctx context.Context,
	client *julesClient,
	args []string,
) error {
	flags := flag.NewFlagSet("daily-finalize", flag.ContinueOnError)
	input := flags.String("input", "", "manifesto JSON da finalização")
	outputDir := flags.String("output-dir", "", "diretório privado de recibos")
	allowedActor := flags.String(
		"allowed-actor",
		"",
		"login GitHub observado e autorizado para o Jules",
	)
	confirmedPR := flags.Int(
		"confirm-consolidated-pr",
		0,
		"número exato do PR humano confirmado",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *outputDir == "" || *allowedActor == "" {
		return errors.New(
			"--input, --output-dir e --allowed-actor são obrigatórios",
		)
	}

	var manifest dailyFinalizationManifest
	if err := readDailyFinalizationPrivateJSON(*input, &manifest); err != nil {
		return err
	}
	if manifest.ConsolidatedPR <= 0 ||
		*confirmedPR != manifest.ConsolidatedPR {
		return errors.New(
			"--confirm-consolidated-pr deve corresponder ao manifesto",
		)
	}
	if len(manifest.AuditPaths) == 0 {
		return errors.New("manifesto não contém auditorias")
	}

	consolidated, err := loadDailyFinalizationConsolidatedPR(
		manifest.ConsolidatedPR,
	)
	if err != nil {
		return err
	}
	if err := validateDailyFinalizationConsolidatedPR(
		consolidated,
		manifest.ConsolidatedPR,
	); err != nil {
		return err
	}

	seenSessions := make(map[string]struct{}, len(manifest.AuditPaths))
	seenPRs := make(map[int]struct{}, len(manifest.AuditPaths))
	audits := make([]dailyFinalizationAuditInput, 0, len(manifest.AuditPaths))
	for _, auditPath := range manifest.AuditPaths {
		var audit dailyPRAudit
		if err := readDailyFinalizationPrivateJSON(auditPath, &audit); err != nil {
			return err
		}
		if err := validateDailyPRAudit(audit, *allowedActor); err != nil {
			return fmt.Errorf("auditoria %s: %w", auditPath, err)
		}
		if err := validateDailyFinalizationItem(
			audit,
			manifest.ConsolidatedPR,
		); err != nil {
			return fmt.Errorf("auditoria %s: %w", auditPath, err)
		}
		if _, duplicate := seenSessions[audit.SessionID]; duplicate {
			return fmt.Errorf("sessionId duplicado: %s", audit.SessionID)
		}
		if _, duplicate := seenPRs[audit.PRNumber]; duplicate {
			return fmt.Errorf("PR duplicado: %d", audit.PRNumber)
		}
		seenSessions[audit.SessionID] = struct{}{}
		seenPRs[audit.PRNumber] = struct{}{}
		audits = append(audits, dailyFinalizationAuditInput{
			Path:  auditPath,
			Audit: audit,
		})
	}
	for index := range audits {
		revision, preflightErr := preflightDailySourcePR(
			ctx,
			client,
			audits[index].Audit,
			audits[index].Path,
			consolidated,
			*allowedActor,
			*outputDir,
		)
		if preflightErr != nil {
			return preflightErr
		}
		audits[index].Revision = revision
	}
	for _, item := range audits {
		if err := finalizeDailySourcePR(
			item.Audit,
			consolidated,
			*outputDir,
			item.Revision,
		); err != nil {
			return err
		}
	}
	return nil
}

func preflightDailySourcePR(
	ctx context.Context,
	client *julesClient,
	audit dailyPRAudit,
	auditPath string,
	consolidated dailyFinalizationConsolidatedPR,
	allowedActor string,
	outputDir string,
) (dailyIssueRevision, error) {
	receiptPath := dailyFinalizationReceiptPath(
		outputDir,
		audit.SessionID,
		audit.PRNumber,
	)
	var receipt dailyFinalizationReceipt
	if _, err := os.Stat(receiptPath); err == nil {
		if err := readDailyFinalizationPrivateJSON(receiptPath, &receipt); err != nil {
			return dailyIssueRevision{}, err
		}
		if err := validateDailyFinalizationReceipt(
			receipt,
			audit.SessionID,
			audit.PRNumber,
			consolidated.Number,
		); err != nil {
			return dailyIssueRevision{}, err
		}
		return dailyIssueRevision{
			Number:    receipt.IssueNumber,
			UpdatedAt: receipt.IssueUpdatedAt,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return dailyIssueRevision{}, fmt.Errorf(
			"consultar recibo %s: %w",
			receiptPath,
			err,
		)
	}
	if err := runDailyPRAuditLiveForFinalization([]string{
		"--input", auditPath,
		"--allowed-actor", allowedActor,
	}); err != nil {
		return dailyIssueRevision{}, err
	}
	sessionRaw, requestErr := client.do(
		ctx,
		http.MethodGet,
		sessionsEndpoint+"/"+audit.SessionID+"?fields="+
			urlQueryEscape(finalizationSessionFields),
		nil,
	)
	if requestErr != nil {
		return dailyIssueRevision{}, fmt.Errorf(
			"consultar sessão %s: %w",
			audit.SessionID,
			requestErr,
		)
	}
	session, parseErr := parseSession(sessionRaw)
	if parseErr != nil {
		return dailyIssueRevision{}, parseErr
	}
	if !strings.HasPrefix(session.Title, dailyTitlePrefix) ||
		sessionPullRequestURL(session.Raw) != audit.PRURL {
		return dailyIssueRevision{}, errors.New(
			"sessão Jules diverge do PR auditado",
		)
	}
	revision, err := issueRevisionFromSession(session)
	if err != nil {
		return dailyIssueRevision{}, err
	}
	return revision, nil
}

func validateDailyFinalizationItem(
	audit dailyPRAudit,
	consolidatedPR int,
) error {
	switch audit.Disposition {
	case "absorbed":
		if audit.ConsolidatedPR != consolidatedPR ||
			!audit.ConsolidatedCIGreen {
			return errors.New(
				"absorbed diverge do PR consolidado verde",
			)
		}
	case "duplicate", "invalid", "no_value", "blocked", "rejected_security":
	default:
		return errors.New(
			"finalização exige disposição terminal, não accepted/needs_revision",
		)
	}
	return nil
}

func loadDailyFinalizationConsolidatedPR(
	prNumber int,
) (dailyFinalizationConsolidatedPR, error) {
	raw, err := runGitHubCLI(
		"pr", "view", strconv.Itoa(prNumber),
		"--repo", "emersonbusson/ramshared",
		"--json", "number,url,baseRefName,headRefOid,state,mergedAt,statusCheckRollup",
	)
	if err != nil {
		return dailyFinalizationConsolidatedPR{}, err
	}
	var live dailyFinalizationConsolidatedPR
	if err := json.Unmarshal(raw, &live); err != nil {
		return live, fmt.Errorf("decodificar PR consolidado: %w", err)
	}
	return live, nil
}

func validateDailyFinalizationConsolidatedPR(
	live dailyFinalizationConsolidatedPR,
	expectedPR int,
) error {
	expectedURL := fmt.Sprintf(
		"https://github.com/emersonbusson/ramshared/pull/%d",
		expectedPR,
	)
	if live.Number != expectedPR || live.URL != expectedURL ||
		live.BaseRefName != "main" || !isFullGitSHA(live.HeadRefOID) {
		return errors.New("identidade do PR consolidado diverge do manifesto")
	}
	if live.State != "OPEN" && !(live.State == "MERGED" && live.MergedAt != "") {
		return errors.New("PR consolidado deve estar aberto ou mergeado")
	}
	if len(live.StatusCheckRollup) == 0 {
		return errors.New("PR consolidado não possui CI observável")
	}
	var mergedAt time.Time
	if live.State == "MERGED" {
		var err error
		mergedAt, err = time.Parse(time.RFC3339, live.MergedAt)
		if err != nil {
			return errors.New("PR consolidado possui mergedAt inválido")
		}
	}
	checks, err := latestDailyFinalizationChecks(live.StatusCheckRollup)
	if err != nil {
		return err
	}
	successful := 0
	for _, check := range checks {
		// O GitHub pode anexar checks de limpeza depois do merge. Eles não
		// existiam no head aprovado e, portanto, não podem invalidar o CI que
		// antecedeu o merge. Timestamp ausente ou anterior continua fail-closed.
		if !mergedAt.IsZero() && check.StartedAt != "" {
			startedAt, err := time.Parse(time.RFC3339, check.StartedAt)
			if err != nil {
				return fmt.Errorf(
					"check %s possui startedAt inválido",
					finalizationCheckName(check),
				)
			}
			if startedAt.After(mergedAt) {
				continue
			}
		}
		status := strings.ToUpper(check.Status)
		conclusion := strings.ToUpper(check.Conclusion)
		state := strings.ToUpper(check.State)
		if state != "" {
			if state != "SUCCESS" {
				return fmt.Errorf(
					"CI do PR consolidado não terminou verde: %s",
					finalizationCheckName(check),
				)
			}
			successful++
			continue
		}
		if status != "COMPLETED" {
			return fmt.Errorf(
				"CI do PR consolidado não terminou verde: %s",
				finalizationCheckName(check),
			)
		}
		switch conclusion {
		case "SUCCESS":
			successful++
		case "SKIPPED", "NEUTRAL":
		default:
			return fmt.Errorf(
				"CI do PR consolidado não terminou verde: %s",
				finalizationCheckName(check),
			)
		}
	}
	if successful == 0 {
		return errors.New("PR consolidado não possui check verde")
	}
	return nil
}

func latestDailyFinalizationChecks(
	checks []dailyFinalizationCheck,
) ([]dailyFinalizationCheck, error) {
	latest := make(map[string]dailyFinalizationCheck, len(checks))
	undated := make([]dailyFinalizationCheck, 0, len(checks))
	for _, check := range checks {
		if check.StartedAt == "" {
			// Sem timestamp não existe prova segura de supersessão.
			undated = append(undated, check)
			continue
		}
		startedAt, err := time.Parse(time.RFC3339, check.StartedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"check %s possui startedAt inválido",
				finalizationCheckName(check),
			)
		}
		key := check.WorkflowName + "\x00" + finalizationCheckName(check)
		current, exists := latest[key]
		if !exists {
			latest[key] = check
			continue
		}
		currentStartedAt, err := time.Parse(time.RFC3339, current.StartedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"check %s possui startedAt inválido",
				finalizationCheckName(current),
			)
		}
		if startedAt.After(currentStartedAt) {
			latest[key] = check
		}
	}
	result := append([]dailyFinalizationCheck(nil), undated...)
	for _, check := range latest {
		result = append(result, check)
	}
	return result, nil
}

func finalizationCheckName(check dailyFinalizationCheck) string {
	if check.Name != "" {
		return check.Name
	}
	if check.Context != "" {
		return check.Context
	}
	return "check sem nome"
}

func finalizeDailySourcePR(
	audit dailyPRAudit,
	consolidated dailyFinalizationConsolidatedPR,
	outputDir string,
	revision dailyIssueRevision,
) error {
	receiptPath := dailyFinalizationReceiptPath(
		outputDir,
		audit.SessionID,
		audit.PRNumber,
	)
	var receipt dailyFinalizationReceipt
	if _, err := os.Stat(receiptPath); err == nil {
		if err := readDailyFinalizationPrivateJSON(receiptPath, &receipt); err != nil {
			return err
		}
		if err := validateDailyFinalizationReceipt(
			receipt,
			audit.SessionID,
			audit.PRNumber,
			consolidated.Number,
		); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("consultar recibo %s: %w", receiptPath, err)
	} else {
		live, liveErr := loadDailyFinalizationSourcePR(audit.PRNumber)
		if liveErr != nil {
			return liveErr
		}
		if live.Number != audit.PRNumber || live.URL != audit.PRURL ||
			(live.State != "OPEN" && live.State != "CLOSED") ||
			live.MergedAt != "" {
			return errors.New(
				"PR de origem mudou após o preflight; finalização interrompida",
			)
		}

		marker := dailyFinalizationCommentMarker(
			audit.PRNumber,
			consolidated.Number,
		)
		comments, ghErr := runGitHubCLI(
			"api",
			fmt.Sprintf(
				"repos/emersonbusson/ramshared/issues/%d/comments",
				audit.PRNumber,
			),
			"--paginate",
			"--jq", ".[].body",
		)
		if ghErr != nil {
			return ghErr
		}
		if !strings.Contains(string(comments), marker) {
			if _, ghErr = runGitHubCLI(
				"pr", "comment", strconv.Itoa(audit.PRNumber),
				"--repo", "emersonbusson/ramshared",
				"--body", dailyFinalizationComment(
					audit.PRNumber,
					consolidated.Number,
					audit.Disposition,
				),
			); ghErr != nil {
				return ghErr
			}
		}
		receipt = dailyFinalizationReceipt{
			Version:             dailyFinalizationMarkerVersion,
			FinalizedAt:         nowUTC().Format(time.RFC3339),
			SessionID:           audit.SessionID,
			SourcePR:            audit.PRNumber,
			SourcePRURL:         audit.PRURL,
			Disposition:         audit.Disposition,
			ConsolidatedPR:      consolidated.Number,
			ConsolidatedPRURL:   consolidated.URL,
			ConsolidatedHeadSHA: consolidated.HeadRefOID,
			CommentMarker:       marker,
			IssueNumber:         revision.Number,
			IssueUpdatedAt:      revision.UpdatedAt,
		}
		if err := writePrivateJSON(receiptPath, receipt); err != nil {
			return fmt.Errorf("gravar recibo antes do fechamento: %w", err)
		}
	}

	live, err := loadDailyFinalizationSourcePR(audit.PRNumber)
	if err != nil {
		return err
	}
	if live.MergedAt != "" {
		return errors.New("PR Jules foi mergeado; finalização interrompida")
	}
	if live.State == "OPEN" {
		if _, err := runGitHubCLI(
			"pr", "close", strconv.Itoa(audit.PRNumber),
			"--repo", "emersonbusson/ramshared",
		); err != nil {
			return err
		}
		live, err = loadDailyFinalizationSourcePR(audit.PRNumber)
		if err != nil {
			return err
		}
	}
	if live.Number != audit.PRNumber || live.URL != audit.PRURL ||
		live.State != "CLOSED" || live.MergedAt != "" {
		return errors.New("fechamento do PR Jules não foi comprovado")
	}
	receipt.PRClosedVerified = true
	if err := writePrivateJSON(receiptPath, receipt); err != nil {
		return fmt.Errorf("gravar prova de fechamento: %w", err)
	}
	return nil
}

func loadDailyFinalizationSourcePR(
	prNumber int,
) (dailyFinalizationSourcePR, error) {
	raw, err := runGitHubCLI(
		"pr", "view", strconv.Itoa(prNumber),
		"--repo", "emersonbusson/ramshared",
		"--json", "number,url,state,mergedAt",
	)
	if err != nil {
		return dailyFinalizationSourcePR{}, err
	}
	var live dailyFinalizationSourcePR
	if err := json.Unmarshal(raw, &live); err != nil {
		return live, fmt.Errorf("decodificar PR de origem: %w", err)
	}
	return live, nil
}

func dailyFinalizationComment(
	sourcePR int,
	consolidatedPR int,
	disposition string,
) string {
	return fmt.Sprintf(
		"%s\n\nEste PR foi encerrado como **superseded**, sem merge. "+
			"A disposição auditada é `%s`; o destino humano da onda é #%d. "+
			"Somente o consolidado segue para `main` após CI integral verde.",
		dailyFinalizationCommentMarker(sourcePR, consolidatedPR),
		disposition,
		consolidatedPR,
	)
}

func dailyFinalizationCommentMarker(sourcePR, consolidatedPR int) string {
	return fmt.Sprintf(
		"<!-- jules-operator:finalized:%s source-pr=%d consolidated-pr=%d -->",
		dailyFinalizationMarkerVersion,
		sourcePR,
		consolidatedPR,
	)
}

func dailyFinalizationReceiptPath(
	dir string,
	sessionID string,
	prNumber int,
) string {
	return filepath.Join(
		dir,
		fmt.Sprintf("session-%s-pr-%d.json", sessionID, prNumber),
	)
}

func validateDailyFinalizationReceipt(
	receipt dailyFinalizationReceipt,
	sessionID string,
	prNumber int,
	consolidatedPR int,
) error {
	if prNumber <= 0 || consolidatedPR <= 0 || prNumber == consolidatedPR {
		return errors.New("recibo possui números de PR inválidos")
	}
	if _, err := time.Parse(time.RFC3339, receipt.FinalizedAt); err != nil {
		return errors.New("recibo possui finalizedAt inválido")
	}
	switch receipt.Disposition {
	case "absorbed", "duplicate", "invalid", "no_value", "blocked", "rejected_security":
	default:
		return errors.New("recibo possui disposição não terminal")
	}
	if receipt.Version != dailyFinalizationMarkerVersion ||
		receipt.SessionID != sessionID ||
		receipt.SourcePR != prNumber ||
		receipt.SourcePRURL != fmt.Sprintf(
			"https://github.com/emersonbusson/ramshared/pull/%d",
			prNumber,
		) ||
		receipt.ConsolidatedPR != consolidatedPR ||
		receipt.ConsolidatedPRURL != fmt.Sprintf(
			"https://github.com/emersonbusson/ramshared/pull/%d",
			consolidatedPR,
		) ||
		!isFullGitSHA(receipt.ConsolidatedHeadSHA) ||
		receipt.CommentMarker != dailyFinalizationCommentMarker(
			prNumber,
			consolidatedPR,
		) {
		return errors.New("recibo de finalização diverge da onda")
	}
	if (receipt.IssueNumber == 0) != (receipt.IssueUpdatedAt == "") {
		return errors.New("recibo possui revisão parcial do issue")
	}
	if receipt.IssueNumber < 0 {
		return errors.New("recibo possui issueNumber inválido")
	}
	if receipt.IssueUpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339, receipt.IssueUpdatedAt); err != nil {
			return errors.New("recibo possui issueUpdatedAt inválido")
		}
	}
	return nil
}

func loadHandledIssueRevisions(
	dir string,
) (map[dailyIssueRevision]struct{}, error) {
	handled := make(map[dailyIssueRevision]struct{})
	if dir == "" {
		return handled, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return handled, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ler recibos de finalização: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "session-") ||
			!strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		var receipt dailyFinalizationReceipt
		if err := readDailyFinalizationPrivateJSON(path, &receipt); err != nil {
			return nil, err
		}
		if err := validateDailyFinalizationReceipt(
			receipt,
			receipt.SessionID,
			receipt.SourcePR,
			receipt.ConsolidatedPR,
		); err != nil {
			return nil, fmt.Errorf("recibo %s inválido: %w", path, err)
		}
		if !receipt.PRClosedVerified || receipt.IssueNumber == 0 {
			continue
		}
		handled[dailyIssueRevision{
			Number:    receipt.IssueNumber,
			UpdatedAt: receipt.IssueUpdatedAt,
		}] = struct{}{}
	}
	return handled, nil
}

func loadFinalizedSessionIDs(dir string) (map[string]struct{}, error) {
	finalized := make(map[string]struct{})
	if dir == "" {
		return finalized, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return finalized, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ler recibos de finalização: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "session-") ||
			!strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		var receipt dailyFinalizationReceipt
		if err := readDailyFinalizationPrivateJSON(path, &receipt); err != nil {
			return nil, err
		}
		if err := validateDailyFinalizationReceipt(
			receipt,
			receipt.SessionID,
			receipt.SourcePR,
			receipt.ConsolidatedPR,
		); err != nil {
			return nil, fmt.Errorf("recibo %s inválido: %w", path, err)
		}
		if !receipt.PRClosedVerified {
			continue
		}
		if _, duplicate := finalized[receipt.SessionID]; duplicate {
			return nil, fmt.Errorf(
				"sessão %s possui mais de um recibo finalizado",
				receipt.SessionID,
			)
		}
		finalized[receipt.SessionID] = struct{}{}
	}
	return finalized, nil
}

func readDailyFinalizationPrivateJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("consultar arquivo privado %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf(
			"arquivo privado inseguro: %s deve ser regular, owner atual e modo 0600",
			path,
		)
	}
	return readJSONFile(path, target)
}
