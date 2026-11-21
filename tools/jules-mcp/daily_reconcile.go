package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	dailyReconcileActionLimit = 20
	dailyContinuationLimit    = 3
	dailyContinuationMarker   = "JULES_DAILY_CONTINUE_V1"
	dailyStalledThreshold     = 15 * time.Minute
)

const dailyContinuationPrompt = dailyContinuationMarker + `

Continue a tarefa de forma autônoma e limitada ao issue e ao repositório
observados. Não peça credenciais, não altere governança ou workflows, não use
Actions, não faça merge e não saia de jules/inbox. Se a implementação segura
não for possível, crie um PR FINDING_ONLY apenas em
docs/jules/findings/<day>/issue-<n>-<fingerprint>.md. Em qualquer caso, faça
commit e abra o PR de intake, registrando comandos e resultados exatos.`

type dailyReconcileAction struct {
	SessionID   string `json:"sessionId"`
	State       string `json:"state"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type dailyReconcileArtifact struct {
	GeneratedAt        string                 `json:"generatedAt"`
	Observed           int                    `json:"observed"`
	Working            int                    `json:"working"`
	Stalled            int                    `json:"stalled"`
	WaitingPlan        int                    `json:"waitingPlan"`
	WaitingFeedback    int                    `json:"waitingFeedback"`
	Failed             int                    `json:"failed"`
	CompletedWithPR    int                    `json:"completedWithPr"`
	CompletedWithoutPR int                    `json:"completedWithoutPr"`
	MessagesSent       int                    `json:"messagesSent"`
	PlansApproved      int                    `json:"plansApproved"`
	ManualRequired     int                    `json:"manualRequired"`
	Actions            []dailyReconcileAction `json:"actions"`
}

type dailyReconcileActivity struct {
	CreateTime    string `json:"createTime"`
	AgentMessaged *struct {
		AgentMessage string `json:"agentMessage"`
	} `json:"agentMessaged"`
	UserMessaged *struct {
		UserMessage string `json:"userMessage"`
	} `json:"userMessaged"`
}

func dailyReconcileReadFailure(
	session sessionRecord,
	err error,
) (dailyReconcileAction, bool) {
	if !errors.Is(err, errJulesResponseTooLarge) {
		return dailyReconcileAction{}, false
	}
	return dailyReconcileAction{
		SessionID:   session.ID,
		State:       session.State,
		Disposition: "manual_activity_too_large",
		Detail:      "atividade excede o teto dedicado de 32 MiB; demais sessões continuam",
	}, true
}

func runDailyReconcile(
	ctx context.Context,
	client *julesClient,
	args []string,
	now time.Time,
) error {
	flags := flag.NewFlagSet("daily-reconcile", flag.ContinueOnError)
	output := flags.String("output", "", "artefato do reconciliador")
	confirmPrefix := flags.String("confirm-prefix", "", "prefixo autorizado")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output é obrigatório")
	}
	if *confirmPrefix != dailyTitlePrefix {
		return fmt.Errorf(
			"--confirm-prefix deve ser exatamente %q",
			dailyTitlePrefix,
		)
	}
	sessions, err := listAllSessions(ctx, client)
	if err != nil {
		return err
	}
	artifact := dailyReconcileArtifact{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Actions:     make([]dailyReconcileAction, 0),
	}
	for _, session := range sessions {
		if !strings.HasPrefix(session.Title, dailyTitlePrefix) ||
			session.CreateTime.IsZero() ||
			session.CreateTime.Before(now.Add(-dailyQuotaWindow)) {
			continue
		}
		artifact.Observed++
		if len(artifact.Actions) >= dailyReconcileActionLimit {
			artifact.ManualRequired++
			continue
		}
		switch session.State {
		case julesStateFailed:
			artifact.Failed++
			artifact.ManualRequired++
			artifact.Actions = append(artifact.Actions, dailyReconcileAction{
				SessionID:   session.ID,
				State:       session.State,
				Disposition: "terminal_failed",
				Detail:      "sessão terminal; continuação é impossível e retry exige nova sessão classificada",
			})
			continue
		case julesStateCompleted:
			if sessionPullRequestURL(session.Raw) != "" {
				artifact.CompletedWithPR++
				continue
			}
			artifact.CompletedWithoutPR++
			artifact.ManualRequired++
			artifact.Actions = append(artifact.Actions, dailyReconcileAction{
				SessionID:   session.ID,
				State:       session.State,
				Disposition: "completed_without_pr",
				Detail:      "sessão terminou sem PR; retry exige nova sessão classificada",
			})
			continue
		}
		activities, requestErr := listAllReconcileSignals(ctx, client, session.ID)
		if requestErr != nil {
			if action, manual := dailyReconcileReadFailure(session, requestErr); manual {
				artifact.ManualRequired++
				artifact.Actions = append(artifact.Actions, action)
				continue
			}
			return fmt.Errorf("ler atividades de %s: %w", session.ID, requestErr)
		}
		switch session.State {
		case julesStateAwaitingPlan:
			artifact.WaitingPlan++
			metric, metricErr := measureLatestAutoPRPlan(session, activities)
			if metricErr != nil {
				return fmt.Errorf("medir plano de %s: %w", session.ID, metricErr)
			}
			if !metric.EligibleForReview {
				artifact.ManualRequired++
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "manual_plan",
					Detail:      strings.Join(metric.Missing, ", "),
				})
				continue
			}
			if _, requestErr := client.do(
				ctx,
				http.MethodPost,
				sessionsEndpoint+"/"+session.ID+":approvePlan",
				map[string]string{},
			); requestErr != nil {
				return fmt.Errorf("aprovar plano de %s: %w", session.ID, requestErr)
			}
			artifact.PlansApproved++
			artifact.Actions = append(artifact.Actions, dailyReconcileAction{
				SessionID:   session.ID,
				State:       session.State,
				Disposition: "approved_plan",
				Detail:      "plano passou nos dez gates de execução e fronteira de PR",
			})
		case julesStateAwaitingUser:
			artifact.WaitingFeedback++
			messages, parseErr := parseDailyReconcileActivities(activities)
			if parseErr != nil {
				return fmt.Errorf("interpretar feedback de %s: %w", session.ID, parseErr)
			}
			lastChange := session.UpdateTime
			if lastChange.IsZero() {
				lastChange = session.CreateTime
			}
			if messages.latestWasContinuation &&
				now.Sub(lastChange) < dailyStalledThreshold {
				continue
			}
			if messages.continuationCount >= dailyContinuationLimit {
				artifact.ManualRequired++
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "manual_exhausted",
					Detail:      "três retomadas automáticas não produziram PR",
				})
				continue
			}
			if messages.dangerous ||
				(messages.latestAgentMessage == "" && !messages.latestWasContinuation) {
				artifact.ManualRequired++
				detail := "feedback exige decisão humana"
				if messages.latestAgentMessage == "" {
					detail = "estado de feedback sem mensagem do agente"
				}
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "manual_safety",
					Detail:      detail,
				})
				continue
			}
			if _, requestErr := client.do(
				ctx,
				http.MethodPost,
				sessionsEndpoint+"/"+session.ID+":sendMessage",
				map[string]string{"prompt": dailyContinuationPrompt},
			); requestErr != nil {
				return fmt.Errorf("continuar sessão %s: %w", session.ID, requestErr)
			}
			artifact.MessagesSent++
			artifact.Actions = append(artifact.Actions, dailyReconcileAction{
				SessionID:   session.ID,
				State:       session.State,
				Disposition: "continued",
				Detail:      "retomada segura enviada; limite de três por sessão",
			})
		case julesStateQueued, julesStateInProgress:
			messages, parseErr := parseDailyReconcileActivities(activities)
			if parseErr != nil {
				return fmt.Errorf("interpretar progresso de %s: %w", session.ID, parseErr)
			}
			lastChange := messages.latestActivityTime
			if lastChange.IsZero() {
				lastChange = session.CreateTime
			}
			if !messages.latestWasContinuation &&
				looksLikeBlockingQuestion(messages.latestAgentMessage) {
				if messages.dangerous {
					artifact.ManualRequired++
					artifact.Actions = append(artifact.Actions, dailyReconcileAction{
						SessionID:   session.ID,
						State:       session.State,
						Disposition: "manual_safety",
						Detail:      "pergunta implícita solicita ação perigosa",
					})
					continue
				}
				if messages.continuationCount >= dailyContinuationLimit {
					artifact.ManualRequired++
					artifact.Actions = append(artifact.Actions, dailyReconcileAction{
						SessionID:   session.ID,
						State:       session.State,
						Disposition: "manual_exhausted",
						Detail:      "três retomadas automáticas não produziram PR",
					})
					continue
				}
				if _, requestErr := client.do(
					ctx,
					http.MethodPost,
					sessionsEndpoint+"/"+session.ID+":sendMessage",
					map[string]string{"prompt": dailyContinuationPrompt},
				); requestErr != nil {
					return fmt.Errorf(
						"responder pergunta implícita de %s: %w",
						session.ID,
						requestErr,
					)
				}
				artifact.MessagesSent++
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "continued_question",
					Detail:      "pergunta segura detectada sem transição de estado; retomada idempotente enviada",
				})
				continue
			}
			if now.Sub(lastChange) < dailyStalledThreshold {
				artifact.Working++
				continue
			}
			artifact.Stalled++
			if messages.dangerous {
				artifact.ManualRequired++
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "manual_safety",
					Detail:      "sessão travada contém solicitação perigosa",
				})
				continue
			}
			if messages.continuationCount >= dailyContinuationLimit {
				artifact.ManualRequired++
				artifact.Actions = append(artifact.Actions, dailyReconcileAction{
					SessionID:   session.ID,
					State:       session.State,
					Disposition: "manual_exhausted",
					Detail:      "três retomadas automáticas não produziram PR",
				})
				continue
			}
			if _, requestErr := client.do(
				ctx,
				http.MethodPost,
				sessionsEndpoint+"/"+session.ID+":sendMessage",
				map[string]string{"prompt": dailyContinuationPrompt},
			); requestErr != nil {
				return fmt.Errorf("retomar sessão travada %s: %w", session.ID, requestErr)
			}
			artifact.MessagesSent++
			artifact.Actions = append(artifact.Actions, dailyReconcileAction{
				SessionID:   session.ID,
				State:       session.State,
				Disposition: "continued_stalled",
				Detail:      "sessão sem atividade material há 15 minutos recebeu retomada limitada",
			})
		}
	}
	return writePrivateJSON(*output, artifact)
}

type dailyReconcileMessages struct {
	latestAgentMessage    string
	latestWasContinuation bool
	continuationCount     int
	dangerous             bool
	latestActivityTime    time.Time
}

func parseDailyReconcileActivities(raw json.RawMessage) (dailyReconcileMessages, error) {
	var response struct {
		Activities []dailyReconcileActivity `json:"activities"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return dailyReconcileMessages{}, err
	}
	result := dailyReconcileMessages{}
	for _, activity := range response.Activities {
		if activity.CreateTime != "" {
			createdAt, err := time.Parse(time.RFC3339Nano, activity.CreateTime)
			if err != nil {
				return dailyReconcileMessages{}, fmt.Errorf(
					"atividade com createTime inválido: %w",
					err,
				)
			}
			if createdAt.After(result.latestActivityTime) {
				result.latestActivityTime = createdAt
			}
		}
		if activity.UserMessaged != nil &&
			strings.Contains(
				activity.UserMessaged.UserMessage,
				dailyContinuationMarker,
			) {
			result.continuationCount++
			result.latestWasContinuation = true
		}
		if activity.AgentMessaged == nil {
			continue
		}
		result.latestAgentMessage = strings.TrimSpace(
			activity.AgentMessaged.AgentMessage,
		)
		result.latestWasContinuation = false
	}
	result.dangerous = containsDangerousFeedback(result.latestAgentMessage)
	return result, nil
}

func containsDangerousFeedback(message string) bool {
	message = strings.ToLower(message)
	for _, term := range []string{
		"api key", "api-key", "secret", "password", "credential",
		"access token", "gh_token", "github token", "merge", "workflow",
		"actions",
	} {
		if strings.Contains(message, term) {
			return true
		}
	}
	return false
}

func looksLikeBlockingQuestion(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	if strings.Contains(message, "?") {
		return true
	}
	for _, marker := range []string{
		"aguardo orientação",
		"aguardo confirmação",
		"let me know",
		"should i proceed",
		"posso prosseguir",
		"confirme para prosseguir",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
