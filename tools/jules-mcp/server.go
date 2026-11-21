package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type mcpJulesServer struct {
	client        *julesClient
	allowedSource string
	logger        *slog.Logger
}

func newMCPServer(client *julesClient, allowedSource string, logger *slog.Logger) *server.MCPServer {
	if allowedSource == "" {
		allowedSource = allowedSourceName
	}
	s := &mcpJulesServer{client: client, allowedSource: allowedSource, logger: logger}
	mcpServer := server.NewMCPServer("ramshared-jules", "0.1.0", server.WithToolCapabilities(true))
	mcpServer.AddTools(
		server.ServerTool{Tool: readOnlyTool("jules_list_sources", "Lista as fontes Jules disponíveis; não altera estado."), Handler: s.listSources},
		server.ServerTool{Tool: readOnlyTool("jules_list_sessions", "Lista sessões Jules paginadas; não altera estado."), Handler: s.listSessions},
		server.ServerTool{Tool: readOnlyTool("jules_get_session", "Consulta uma sessão Jules por ID; não altera estado."), Handler: s.getSession},
		server.ServerTool{Tool: readOnlyTool("jules_list_activities", "Lista atividades de uma sessão Jules; não altera estado."), Handler: s.listActivities},
		server.ServerTool{Tool: readOnlyTool("jules_measure_plan", "Mede o plano mais recente em 10 requisitos objetivos; não aprova nem altera estado."), Handler: s.measurePlan},
		server.ServerTool{Tool: destructiveTool("jules_create_session", "Cria uma sessão Jules somente com confirm=true e na fonte allowlisted."), Handler: s.createSession},
		server.ServerTool{Tool: destructiveTool("jules_send_message", "Envia uma mensagem a Jules somente com confirm=true."), Handler: s.sendMessage},
		server.ServerTool{Tool: destructiveTool("jules_approve_plan", "Aprova um plano Jules somente com confirm=true."), Handler: s.approvePlan},
		server.ServerTool{Tool: destructiveTool("jules_delete_session", "Exclui uma sessão Jules somente com confirm=true."), Handler: s.deleteSession},
	)
	return mcpServer
}

func readOnlyTool(name, description string) mcp.Tool {
	tool := mcp.NewTool(name, mcp.WithDescription(description))
	tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(true)
	tool.Annotations.DestructiveHint = mcp.ToBoolPtr(false)
	return tool
}

func destructiveTool(name, description string) mcp.Tool {
	tool := mcp.NewTool(name, mcp.WithDescription(description), mcp.WithBoolean("confirm", mcp.Description("Confirma explicitamente a operação mutável.")))
	tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(false)
	tool.Annotations.DestructiveHint = mcp.ToBoolPtr(true)
	return tool
}

func (s *mcpJulesServer) call(ctx context.Context, method, path string, body any) (*mcp.CallToolResult, error) {
	result, err := s.client.do(ctx, method, path, body)
	if err != nil {
		s.logger.Warn("jules request failed", "method", method, "path", path, "error", err.Error())
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultJSON(result)
}

func requireID(req mcp.CallToolRequest) (string, error) {
	id, err := req.RequireString("sessionId")
	if err != nil || !sessionIDPattern.MatchString(id) {
		return "", errors.New("sessionId inválido")
	}
	return id, nil
}

func requireConfirm(req mcp.CallToolRequest) error {
	if !req.GetBool("confirm", false) {
		return errors.New("operação mutável exige confirm=true")
	}
	return nil
}

func (s *mcpJulesServer) listSources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.call(ctx, http.MethodGet, "/sources", nil)
}

func (s *mcpJulesServer) listSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pageSize := req.GetInt("pageSize", 30)
	if pageSize < 1 || pageSize > 100 {
		return mcp.NewToolResultError("pageSize deve estar entre 1 e 100"), nil
	}
	path := fmt.Sprintf("/sessions?pageSize=%d", pageSize)
	if token := req.GetString("pageToken", ""); token != "" {
		path += "&pageToken=" + url.QueryEscape(token)
	}
	return s.call(ctx, http.MethodGet, path, nil)
}

func (s *mcpJulesServer) getSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.call(ctx, http.MethodGet, sessionsEndpoint+"/"+id, nil)
}

func (s *mcpJulesServer) listActivities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	pageSize := req.GetInt("pageSize", 30)
	if pageSize < 1 || pageSize > 100 {
		return mcp.NewToolResultError("pageSize deve estar entre 1 e 100"), nil
	}
	return s.call(ctx, http.MethodGet, fmt.Sprintf("/sessions/%s/activities?pageSize=%d", id, pageSize), nil)
}

func (s *mcpJulesServer) measurePlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sessionRaw, err := s.client.do(ctx, http.MethodGet, "/sessions/"+id, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	session, err := parseSession(sessionRaw)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	activitiesRaw, err := s.client.do(ctx, http.MethodGet, "/sessions/"+id+"/activities?pageSize=100", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	metric, err := measureLatestPlan(session, activitiesRaw)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultJSON(metric)
}

func (s *mcpJulesServer) createSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := requireConfirm(req); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil || !validPrompt(prompt) {
		return mcp.NewToolResultError("prompt obrigatório com no máximo 20000 caracteres"), nil
	}
	branch := req.GetString("startingBranch", defaultStartingBranch)
	if branch != defaultStartingBranch && !regexp.MustCompile(`^[A-Za-z0-9._/-]{1,120}$`).MatchString(branch) {
		return mcp.NewToolResultError("startingBranch inválida"), nil
	}
	title := req.GetString("title", "RamShared Jules task")
	if !validTitle(title) {
		return mcp.NewToolResultError("title obrigatório com no máximo 200 caracteres"), nil
	}
	body := map[string]any{"prompt": prompt, "title": title, "requirePlanApproval": true, "sourceContext": map[string]any{"source": s.allowedSource, "githubRepoContext": map[string]any{"startingBranch": branch}}}
	return s.call(ctx, http.MethodPost, sessionsEndpoint, body)
}

func (s *mcpJulesServer) sendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := requireConfirm(req); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil || !validPrompt(prompt) {
		return mcp.NewToolResultError("prompt obrigatório com no máximo 20000 caracteres"), nil
	}
	return s.call(ctx, http.MethodPost, sessionsEndpoint+"/"+id+":sendMessage", map[string]string{"prompt": prompt})
}

func (s *mcpJulesServer) approvePlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := requireConfirm(req); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.call(ctx, http.MethodPost, sessionsEndpoint+"/"+id+":approvePlan", map[string]string{})
}

func (s *mcpJulesServer) deleteSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := requireConfirm(req); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	id, err := requireID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.call(ctx, http.MethodDelete, sessionsEndpoint+"/"+id, nil)
}
