package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
)

const minimumAuditEvidenceCharacters = 20

var allowedDailyPRAuditDispositions = map[string]struct{}{
	"accepted":          {},
	"needs_revision":    {},
	"absorbed":          {},
	"duplicate":         {},
	"invalid":           {},
	"no_value":          {},
	"blocked":           {},
	"rejected_security": {},
}

type dailyPRAuditLens struct {
	Code     string `json:"code"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

type dailyPRAudit struct {
	SessionID           string             `json:"sessionId"`
	PRNumber            int                `json:"prNumber"`
	PRURL               string             `json:"prUrl"`
	BaseBranch          string             `json:"baseBranch"`
	HeadSHA             string             `json:"headSha"`
	MainSHA             string             `json:"mainSha"`
	Author              string             `json:"author"`
	ActionsRunCount     int                `json:"actionsRunCount"`
	Merged              bool               `json:"merged"`
	IntakeKind          string             `json:"intakeKind"`
	BuildStatus         string             `json:"buildStatus"`
	BuildEvidence       string             `json:"buildEvidence"`
	TestStatus          string             `json:"testStatus"`
	TestEvidence        string             `json:"testEvidence"`
	Score               int                `json:"score"`
	SafetyGates         []string           `json:"safetyGates"`
	QualityGates        []string           `json:"qualityGates"`
	Disposition         string             `json:"disposition"`
	ConsolidatedPR      int                `json:"consolidatedPr,omitempty"`
	ConsolidatedCIGreen bool               `json:"consolidatedCiGreen,omitempty"`
	DuplicateOfPR       int                `json:"duplicateOfPr,omitempty"`
	Lenses              []dailyPRAuditLens `json:"lenses"`
}

type dailyPRAuditLivePR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOID  string `json:"headRefOid"`
	State       string `json:"state"`
	MergedAt    string `json:"mergedAt"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
}

type dailyPRAuditActionRun struct {
	DatabaseID int64 `json:"databaseId"`
}

var runGitHubCLI = func(args ...string) ([]byte, error) {
	command := exec.Command("gh", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"gh %s: %w: %s",
			strings.Join(args, " "),
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return output, nil
}

func runDailyPRAuditValidate(args []string) error {
	flags := flag.NewFlagSet("daily-pr-audit-validate", flag.ContinueOnError)
	input := flags.String("input", "", "artefato JSON da auditoria")
	allowedActor := flags.String(
		"allowed-actor",
		"",
		"login GitHub observado e autorizado para o Jules",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *allowedActor == "" {
		return errors.New("--input e --allowed-actor são obrigatórios")
	}
	var audit dailyPRAudit
	if err := readJSONFile(*input, &audit); err != nil {
		return err
	}
	return validateDailyPRAudit(audit, *allowedActor)
}

func runDailyPRAuditLive(args []string) error {
	return runDailyPRAuditLiveWithSourceState(args, false)
}

func runDailyPRAuditLiveForFinalization(args []string) error {
	return runDailyPRAuditLiveWithSourceState(args, true)
}

func runDailyPRAuditLiveWithSourceState(
	args []string,
	allowClosed bool,
) error {
	flags := flag.NewFlagSet("daily-pr-audit-live", flag.ContinueOnError)
	input := flags.String("input", "", "artefato JSON da auditoria")
	allowedActor := flags.String(
		"allowed-actor",
		"",
		"login GitHub observado e autorizado para o Jules",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *allowedActor == "" {
		return errors.New("--input e --allowed-actor são obrigatórios")
	}
	var audit dailyPRAudit
	if err := readJSONFile(*input, &audit); err != nil {
		return err
	}
	if err := validateDailyPRAudit(audit, *allowedActor); err != nil {
		return err
	}
	liveRaw, err := runGitHubCLI(
		"pr", "view", fmt.Sprint(audit.PRNumber),
		"--repo", "emersonbusson/ramshared",
		"--json", "number,url,baseRefName,headRefOid,state,mergedAt,author,files",
	)
	if err != nil {
		return err
	}
	var live dailyPRAuditLivePR
	if err := json.Unmarshal(liveRaw, &live); err != nil {
		return fmt.Errorf("decodificar PR observado: %w", err)
	}
	runsRaw, err := runGitHubCLI(
		"run", "list",
		"--repo", "emersonbusson/ramshared",
		"--commit", audit.HeadSHA,
		"--limit", "1",
		"--json", "databaseId",
	)
	if err != nil {
		return err
	}
	var runs []dailyPRAuditActionRun
	if err := json.Unmarshal(runsRaw, &runs); err != nil {
		return fmt.Errorf("decodificar runs observados: %w", err)
	}
	mainRaw, err := runGitHubCLI(
		"api", "repos/emersonbusson/ramshared/git/ref/heads/main",
		"--jq", ".object.sha",
	)
	if err != nil {
		return err
	}
	return validateDailyPRAuditLiveWithSourceState(
		audit, live, runs, strings.TrimSpace(string(mainRaw)), allowClosed,
	)
}

func validateDailyPRAuditLive(
	audit dailyPRAudit,
	live dailyPRAuditLivePR,
	runs []dailyPRAuditActionRun,
	mainSHA string,
) error {
	return validateDailyPRAuditLiveWithSourceState(
		audit, live, runs, mainSHA, false,
	)
}

func validateDailyPRAuditLiveWithSourceState(
	audit dailyPRAudit,
	live dailyPRAuditLivePR,
	runs []dailyPRAuditActionRun,
	mainSHA string,
	allowClosed bool,
) error {
	if live.Number != audit.PRNumber ||
		live.URL != audit.PRURL ||
		live.BaseRefName != audit.BaseBranch ||
		live.HeadRefOID != audit.HeadSHA ||
		live.Author.Login != audit.Author {
		return errors.New("PR observado diverge da identidade auditada")
	}
	if (live.State != "OPEN" && !(allowClosed && live.State == "CLOSED")) ||
		live.MergedAt != "" {
		return errors.New("PR observado deve estar aberto e não mergeado")
	}
	for _, file := range live.Files {
		if isJulesGovernancePath(file.Path) {
			return fmt.Errorf(
				"PR Jules alterou caminho de governança proibido: %s",
				file.Path,
			)
		}
	}
	if audit.IntakeKind == "finding_only" {
		if len(live.Files) != 1 ||
			!strings.HasPrefix(live.Files[0].Path, "docs/jules/findings/") ||
			!strings.HasSuffix(live.Files[0].Path, ".md") {
			return errors.New(
				"finding_only deve alterar 1 Markdown sob docs/jules/findings",
			)
		}
	}
	if len(runs) != 0 {
		return fmt.Errorf(
			"head SHA observado executou ao menos %d run(s) de Actions",
			len(runs),
		)
	}
	if !isFullGitSHA(mainSHA) || (!allowClosed && mainSHA != audit.MainSHA) {
		return errors.New("mainSha diverge da main observada")
	}
	return nil
}

func isJulesGovernancePath(path string) bool {
	switch path {
	case "AGENTS.md", "CLAUDE.md", "CODEX.md":
		return true
	}
	return strings.HasPrefix(path, ".github/workflows/") ||
		strings.HasPrefix(path, ".claude/rules/")
}

func validateDailyPRAudit(audit dailyPRAudit, allowedActor string) error {
	if !sessionIDPattern.MatchString(audit.SessionID) {
		return errors.New("sessionId inválido")
	}
	if audit.PRNumber <= 0 {
		return errors.New("prNumber deve ser positivo")
	}
	expectedURL := fmt.Sprintf(
		"https://github.com/emersonbusson/ramshared/pull/%d",
		audit.PRNumber,
	)
	if audit.PRURL != expectedURL {
		return errors.New("prUrl não corresponde ao PR auditado")
	}
	if audit.BaseBranch != julesInboxBranch {
		return fmt.Errorf("baseBranch deve ser %s", julesInboxBranch)
	}
	if allowedActor == "" || audit.Author != allowedActor {
		return errors.New("author não corresponde ao ator Jules autorizado")
	}
	if !isFullGitSHA(audit.HeadSHA) || !isFullGitSHA(audit.MainSHA) {
		return errors.New("headSha e mainSha devem ter 40 caracteres hexadecimais")
	}
	if audit.ActionsRunCount != 0 {
		return fmt.Errorf(
			"PR de intake executou %d run(s) de Actions",
			audit.ActionsRunCount,
		)
	}
	if audit.Merged {
		return errors.New("PR de intake não pode estar merged")
	}
	switch audit.IntakeKind {
	case "code_candidate", "finding_only":
	default:
		return errors.New("intakeKind inválido")
	}
	if err := validateDailyExecutionEvidence(audit); err != nil {
		return err
	}
	if audit.Score < 0 || audit.Score > 16 {
		return errors.New("score deve estar entre 0 e 16")
	}
	if _, allowed := allowedDailyPRAuditDispositions[audit.Disposition]; !allowed {
		return errors.New("disposition inválida")
	}
	for name, gates := range map[string][]string{
		"safetyGates":  audit.SafetyGates,
		"qualityGates": audit.QualityGates,
	} {
		for _, gate := range gates {
			if strings.TrimSpace(gate) == "" {
				return fmt.Errorf("%s não aceita item vazio", name)
			}
		}
	}
	if err := validateDailyPRAuditLenses(audit.Lenses); err != nil {
		return err
	}
	return validateDailyPRAuditDisposition(audit)
}

func validateDailyExecutionEvidence(audit dailyPRAudit) error {
	for name, value := range map[string]string{
		"buildStatus": audit.BuildStatus,
		"testStatus":  audit.TestStatus,
	} {
		switch value {
		case "pass", "fail", "not_applicable":
		default:
			return fmt.Errorf("%s inválido", name)
		}
	}
	for name, evidence := range map[string]string{
		"buildEvidence": audit.BuildEvidence,
		"testEvidence":  audit.TestEvidence,
	} {
		if len([]rune(strings.TrimSpace(evidence))) <
			minimumAuditEvidenceCharacters {
			return fmt.Errorf("%s insuficiente", name)
		}
	}
	if audit.IntakeKind == "finding_only" &&
		(audit.BuildStatus != "not_applicable" ||
			audit.TestStatus != "not_applicable") {
		return errors.New(
			"finding_only exige buildStatus e testStatus not_applicable",
		)
	}
	return nil
}

func validateDailyPRAuditLenses(lenses []dailyPRAuditLens) error {
	if len(lenses) != len(dailyLenses) {
		return fmt.Errorf(
			"auditoria deve conter exatamente %d lentes",
			len(dailyLenses),
		)
	}
	expected := make(map[string]struct{}, len(dailyLenses))
	for _, lens := range dailyLenses {
		expected[lens.Code] = struct{}{}
	}
	seen := make(map[string]struct{}, len(lenses))
	for _, lens := range lenses {
		if _, exists := expected[lens.Code]; !exists {
			return fmt.Errorf("lente desconhecida: %s", lens.Code)
		}
		if _, duplicate := seen[lens.Code]; duplicate {
			return fmt.Errorf("lente duplicada: %s", lens.Code)
		}
		seen[lens.Code] = struct{}{}
		switch lens.Verdict {
		case "pass", "fail", "not_applicable":
		default:
			return fmt.Errorf("verdict inválido na lente %s", lens.Code)
		}
		if len([]rune(strings.TrimSpace(lens.Evidence))) <
			minimumAuditEvidenceCharacters {
			return fmt.Errorf("evidência insuficiente na lente %s", lens.Code)
		}
		if strings.Contains(
			strings.ToLower(lens.Evidence),
			"substitua por",
		) {
			return fmt.Errorf("evidência placeholder na lente %s", lens.Code)
		}
	}
	return nil
}

func validateDailyPRAuditDisposition(audit dailyPRAudit) error {
	switch audit.Disposition {
	case "accepted":
		if audit.Score < 14 ||
			len(audit.SafetyGates) > 0 ||
			len(audit.QualityGates) > 0 {
			return errors.New(
				"accepted exige score >=14 e 0 safety/quality gates",
			)
		}
		if audit.BuildStatus == "fail" || audit.TestStatus == "fail" {
			return errors.New("accepted não permite build ou teste vermelho")
		}
		for _, lens := range audit.Lenses {
			if lens.Verdict == "fail" {
				return errors.New("accepted não permite lente com verdict fail")
			}
		}
	case "absorbed":
		if audit.ConsolidatedPR <= 0 || !audit.ConsolidatedCIGreen {
			return errors.New(
				"absorbed exige consolidatedPr positivo e consolidatedCiGreen=true",
			)
		}
	case "duplicate":
		if audit.DuplicateOfPR <= 0 {
			return errors.New("duplicate exige duplicateOfPr positivo")
		}
	case "rejected_security":
		if len(audit.SafetyGates) == 0 {
			return errors.New(
				"rejected_security exige ao menos 1 safety gate",
			)
		}
	}
	return nil
}

func isFullGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
