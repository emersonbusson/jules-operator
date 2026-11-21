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
	"time"
)

type dailySessionArchive struct {
	ArchivedAt       string          `json:"archivedAt"`
	Session          sessionRecord   `json:"session"`
	Activities       json.RawMessage `json:"activities"`
	ActivitiesStatus string          `json:"activitiesStatus"`
	PullRequestURL   string          `json:"pullRequestUrl"`
	PromptVersion    string          `json:"promptVersion"`
	Lens             string          `json:"lens"`
	Disposition      string          `json:"disposition"`
	ConsolidatedPR   int             `json:"consolidatedPr,omitempty"`
	DeleteVerified   bool            `json:"deleteVerified"`
}

const (
	dailyActivitiesStatusCaptured        = "captured"
	dailyActivitiesStatusBounded         = "captured_bounded_projection"
	dailyActivitiesStatusNotFound        = "not_found"
	dailyActivitiesStatusSessionNotFound = "session_not_found"
	dailyReapActivityArchiveFields       = "nextPageToken,activities(createTime,planApproved)"
)

func runDailyReap(
	ctx context.Context,
	client *julesClient,
	args []string,
) error {
	flags := flag.NewFlagSet("daily-reap", flag.ContinueOnError)
	outputDir := flags.String("output-dir", "", "diretório de evidências")
	finalizationDir := flags.String(
		"finalization-dir",
		"",
		"diretório privado de recibos de finalização",
	)
	confirmPrefix := flags.String("confirm-prefix", "", "prefixo autorizado")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *outputDir == "" {
		return errors.New("--output-dir é obrigatório")
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
	for _, session := range sessions {
		if !strings.HasPrefix(session.Title, dailyTitlePrefix) {
			continue
		}
		// A API não devolve quota ao excluir. Manter a sessão por 24h preserva
		// o contador observável mesmo depois de o PR liberar a concorrência.
		cutoff := nowUTC().Add(-dailyQuotaWindow)
		if session.CreateTime.IsZero() ||
			session.CreateTime.After(cutoff) ||
			(!session.UpdateTime.IsZero() && session.UpdateTime.After(cutoff)) {
			continue
		}
		prURL := sessionPullRequestURL(session.Raw)
		disposition := dailyReapDisposition(session.State, prURL)
		if disposition == "" {
			continue
		}
		var finalization dailyFinalizationReceipt
		if prURL != "" {
			prNumber, ok := pullRequestNumberFromURL(prURL)
			if !ok || *finalizationDir == "" {
				continue
			}
			receiptPath := dailyFinalizationReceiptPath(
				*finalizationDir,
				session.ID,
				prNumber,
			)
			if err := readDailyFinalizationPrivateJSON(
				receiptPath,
				&finalization,
			); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf(
					"validar recibo antes de excluir %s: %w",
					session.ID,
					err,
				)
			}
			if err := validateDailyFinalizationReceipt(
				finalization,
				session.ID,
				prNumber,
				finalization.ConsolidatedPR,
			); err != nil || !finalization.PRClosedVerified {
				return fmt.Errorf(
					"sessão %s sem finalização comprovada: %v",
					session.ID,
					err,
				)
			}
		}
		activities, activitiesStatus, alreadyDeleted, requestErr := dailyReapActivities(
			ctx,
			client,
			session,
			prURL,
		)
		if requestErr != nil {
			return fmt.Errorf(
				"arquivar atividades de %s: %w",
				session.ID,
				requestErr,
			)
		}
		archive := dailySessionArchive{
			ArchivedAt:       nowUTC().Format(time.RFC3339),
			Session:          session,
			Activities:       activities,
			ActivitiesStatus: activitiesStatus,
			PullRequestURL:   prURL,
			PromptVersion:    promptVersionFromSession(session),
			Lens:             lensFromTitle(session.Title),
			Disposition:      disposition,
			ConsolidatedPR:   finalization.ConsolidatedPR,
		}
		path := filepath.Join(*outputDir, "session-"+session.ID+".json")
		if alreadyDeleted {
			archive.DeleteVerified = true
			if err := archiveAlreadyDeletedDailySession(path, archive); err != nil {
				return fmt.Errorf(
					"registrar sessão já excluída %s: %w",
					session.ID,
					err,
				)
			}
			continue
		}
		if err := writePrivateJSON(path, archive); err != nil {
			return fmt.Errorf(
				"arquivar sessão %s antes da exclusão: %w",
				session.ID,
				err,
			)
		}
		if _, requestErr := client.do(
			ctx,
			http.MethodDelete,
			sessionsEndpoint+"/"+session.ID,
			nil,
		); requestErr != nil {
			return fmt.Errorf(
				"excluir sessão arquivada %s: %w",
				session.ID,
				requestErr,
			)
		}
		_, requestErr = client.do(
			ctx,
			http.MethodGet,
			sessionsEndpoint+"/"+session.ID,
			nil,
		)
		if !isJulesHTTPStatus(requestErr, http.StatusNotFound) {
			return fmt.Errorf(
				"exclusão de %s sem prova 404: %v",
				session.ID,
				requestErr,
			)
		}
		archive.DeleteVerified = true
		if err := writePrivateJSON(path, archive); err != nil {
			return fmt.Errorf(
				"registrar prova 404 de %s: %w",
				session.ID,
				err,
			)
		}
	}
	return nil
}

func dailyReapActivities(
	ctx context.Context,
	client *julesClient,
	session sessionRecord,
	pullRequestURL string,
) (json.RawMessage, string, bool, error) {
	activities, err := listAllActivities(ctx, client, session.ID)
	if err == nil {
		return activities, dailyActivitiesStatusCaptured, false, nil
	}
	if errors.Is(err, errJulesResponseTooLarge) {
		bounded, boundedErr := listAllActivitiesWithFields(
			ctx,
			client,
			session.ID,
			dailyReapActivityArchiveFields,
		)
		if boundedErr != nil {
			return nil, "", false, fmt.Errorf(
				"arquivar projeção limitada após atividade grande: %w",
				boundedErr,
			)
		}
		return bounded, dailyActivitiesStatusBounded, false, nil
	}
	if !isJulesHTTPStatus(err, http.StatusNotFound) {
		return nil, "", false, err
	}

	// A listagem pode ficar atrasada depois do DELETE. Antes de considerar a
	// atividade ausente, verificamos a sessão individual para nunca repetir uma
	// exclusão que já tem prova 404 no arquivo local.
	sessionRaw, requestErr := client.do(
		ctx,
		http.MethodGet,
		sessionsEndpoint+"/"+session.ID,
		nil,
	)
	if isJulesHTTPStatus(requestErr, http.StatusNotFound) {
		empty, marshalErr := marshalActivities([]json.RawMessage{})
		if marshalErr != nil {
			return nil, "", false, marshalErr
		}
		return empty, dailyActivitiesStatusSessionNotFound, true, nil
	}
	if requestErr != nil {
		return nil, "", false, fmt.Errorf(
			"confirmar sessão após atividades 404: %w",
			requestErr,
		)
	}
	live, parseErr := parseSession(sessionRaw)
	if parseErr != nil {
		return nil, "", false, parseErr
	}
	if live.ID != session.ID || live.Title != session.Title ||
		live.State != session.State ||
		sessionPullRequestURL(live.Raw) != pullRequestURL {
		return nil, "", false, errors.New(
			"sessão mudou enquanto as atividades estavam indisponíveis",
		)
	}
	empty, marshalErr := marshalActivities([]json.RawMessage{})
	if marshalErr != nil {
		return nil, "", false, marshalErr
	}
	return empty, dailyActivitiesStatusNotFound, false, nil
}

func archiveAlreadyDeletedDailySession(
	path string,
	archive dailySessionArchive,
) error {
	var existing dailySessionArchive
	if err := readDailyFinalizationPrivateJSON(path, &existing); err == nil {
		if existing.Session.ID != archive.Session.ID {
			return errors.New("arquivo existente diverge da sessão excluída")
		}
		if existing.DeleteVerified {
			return nil
		}
		existing.DeleteVerified = true
		return writePrivateJSON(path, existing)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("validar arquivo existente: %w", err)
	}
	return writePrivateJSON(path, archive)
}

func pullRequestNumberFromURL(value string) (int, bool) {
	const prefix = "https://github.com/emersonbusson/ramshared/pull/"
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(value, prefix))
	return number, err == nil && number > 0
}

func dailyReapDisposition(state, pullRequestURL string) string {
	switch state {
	case julesStateAwaitingPlan:
		if pullRequestURL == "" {
			return "expired_unapproved"
		}
	case julesStateAwaitingUser:
		if pullRequestURL == "" {
			return "expired_feedback"
		}
	case julesStatePaused:
		if pullRequestURL == "" {
			return "expired_paused"
		}
	case julesStateFailed:
		if pullRequestURL == "" {
			return "failed"
		}
	case julesStateCompleted:
		if pullRequestURL == "" {
			return "completed_without_pr"
		}
	default:
		return ""
	}
	return "pull_request"
}

func sessionPullRequestURL(raw json.RawMessage) string {
	var payload struct {
		Outputs []struct {
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pullRequest"`
		} `json:"outputs"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	for _, output := range payload.Outputs {
		if output.PullRequest != nil &&
			strings.HasPrefix(
				output.PullRequest.URL,
				"https://github.com/emersonbusson/ramshared/pull/",
			) {
			return output.PullRequest.URL
		}
	}
	return ""
}

func lensFromTitle(title string) string {
	start := strings.Index(title, " L")
	if start < 0 || start+4 > len(title) {
		return ""
	}
	return title[start+1 : start+4]
}

var nowUTC = func() time.Time { return time.Now().UTC() }
