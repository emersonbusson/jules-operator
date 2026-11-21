package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestJulesClientSendsKeyAndNeverLogsOrReturnsIt(t *testing.T) {
	const secret = "test-key-never-returned"
	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-goog-api-key")
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"sessions":[]}`)
	}))
	defer server.Close()

	client, err := newJulesClient(secret, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.do(context.Background(), http.MethodGet, sessionsEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader != secret {
		t.Fatalf("header = %q, want secret", gotHeader)
	}
	if strings.Contains(string(result), secret) {
		t.Fatalf("response exposed API key: %s", result)
	}
}

func TestJulesClientRejectsUntrustedBaseURL(t *testing.T) {
	for _, baseURL := range []string{"http://attacker.example", "https://attacker.example", "https://jules.googleapis.com/v1alpha?x=1"} {
		if _, err := newJulesClient("key", baseURL, http.DefaultClient); err == nil {
			t.Fatalf("base URL %q was accepted", baseURL)
		}
	}
}

func TestReadOnlyAndDestructiveToolsHaveCorrectAnnotations(t *testing.T) {
	readOnly := readOnlyTool("read", "read")
	if readOnly.Annotations.ReadOnlyHint == nil || !*readOnly.Annotations.ReadOnlyHint {
		t.Fatal("read-only tool was not marked readOnlyHint")
	}
	destructive := destructiveTool("write", "write")
	if destructive.Annotations.DestructiveHint == nil || !*destructive.Annotations.DestructiveHint {
		t.Fatal("destructive tool was not marked destructiveHint")
	}
}

func TestMutationsRequireExplicitConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream called before confirmation: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	client, err := newJulesClient("key", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpJulesServer{client: client, allowedSource: allowedSourceName, logger: nilLogger()}
	tools := []struct {
		name string
		args map[string]any
	}{
		{"jules_create_session", map[string]any{"prompt": "test"}},
		{"jules_send_message", map[string]any{"sessionId": "abc", "prompt": "test"}},
		{"jules_approve_plan", map[string]any{"sessionId": "abc"}},
		{"jules_delete_session", map[string]any{"sessionId": "abc"}},
	}
	for _, tc := range tools {
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tc.name, Arguments: tc.args}}
		var result *mcp.CallToolResult
		switch tc.name {
		case "jules_create_session":
			result, err = s.createSession(context.Background(), req)
		case "jules_send_message":
			result, err = s.sendMessage(context.Background(), req)
		case "jules_approve_plan":
			result, err = s.approvePlan(context.Background(), req)
		case "jules_delete_session":
			result, err = s.deleteSession(context.Background(), req)
		}
		if err != nil {
			t.Fatalf("%s protocol error: %v", tc.name, err)
		}
		encoded, _ := json.Marshal(result)
		if !strings.Contains(string(encoded), "confirm=true") {
			t.Fatalf("%s did not explain confirmation requirement: %s", tc.name, encoded)
		}
	}
}

func TestReadOnlyRoutesMatchJulesAPI(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client, err := newJulesClient("key", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpJulesServer{client: client, allowedSource: allowedSourceName, logger: nilLogger()}
	requests := []struct {
		call func() (*mcp.CallToolResult, error)
		args map[string]any
	}{
		{func() (*mcp.CallToolResult, error) { return s.listSources(context.Background(), mcp.CallToolRequest{}) }, nil},
		{func() (*mcp.CallToolResult, error) {
			return s.listSessions(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"pageSize": float64(5), "pageToken": "a&b"}}})
		}, nil},
		{func() (*mcp.CallToolResult, error) {
			return s.getSession(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"sessionId": "abc"}}})
		}, nil},
		{func() (*mcp.CallToolResult, error) {
			return s.listActivities(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"sessionId": "abc", "pageSize": float64(7)}}})
		}, nil},
	}
	for _, request := range requests {
		if _, err := request.call(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"GET /sources", "GET /sessions?pageSize=5&pageToken=a%26b", "GET /sessions/abc", "GET /sessions/abc/activities?pageSize=7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %#v, want %#v", got, want)
	}
}

func TestBatchLaunchRequiresExactCountAndForcesPlanApproval(t *testing.T) {
	var requests int
	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"123","title":"Test","state":"IN_PROGRESS"}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	outputPath := filepath.Join(dir, "launch.json")
	manifest := `{
		"source":"sources/github/emersonbusson/ramshared",
		"startingBranch":"fix/test",
		"requirePlanApproval":true,
		"automationMode":"AUTOMATION_MODE_UNSPECIFIED",
		"commonPrompt":"Leia as regras.",
		"sessions":[{"title":"Test","prompt":"Prove o efeito."}]
	}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runBatch(context.Background(), client, []string{"launch", "--manifest", manifestPath, "--output", outputPath, "--confirm-count", "0"}); err == nil {
		t.Fatal("launch aceitou contagem divergente")
	}
	if requests != 0 {
		t.Fatalf("upstream recebeu %d request(s) antes da confirmação exata", requests)
	}
	if err := runBatch(context.Background(), client, []string{"launch", "--manifest", manifestPath, "--output", outputPath, "--confirm-count", "1"}); err != nil {
		t.Fatal(err)
	}
	if body["requirePlanApproval"] != true {
		t.Fatalf("requirePlanApproval = %#v", body["requirePlanApproval"])
	}
	if _, exists := body["automationMode"]; exists {
		t.Fatal("launch enviou automationMode")
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("modo do artefato = %o", info.Mode().Perm())
	}
}

func TestManifestRejectsAutomaticPullRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	content := `{
		"source":"sources/github/emersonbusson/ramshared",
		"startingBranch":"main",
		"requirePlanApproval":true,
		"automationMode":"AUTO_CREATE_PR",
		"commonPrompt":"rules",
		"sessions":[{"title":"test","prompt":"test"}]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("manifesto aceitou AUTO_CREATE_PR")
	}
}

func TestPromptLimitsCountUnicodeCharacters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"empty", "", false},
		{"whitespace", " \n\t", false},
		{"19999", strings.Repeat("a", maxPromptCharacters-1), true},
		{"20000", strings.Repeat("á", maxPromptCharacters), true},
		{"20001", strings.Repeat("á", maxPromptCharacters+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPrompt(tc.value); got != tc.valid {
				t.Fatalf("validPrompt() = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestTitleLimits(t *testing.T) {
	if !validTitle(strings.Repeat("á", maxTitleCharacters)) {
		t.Fatal("title no limite foi rejeitado")
	}
	if validTitle(strings.Repeat("á", maxTitleCharacters+1)) {
		t.Fatal("title acima do limite foi aceito")
	}
}

func TestJulesClientRejectsOversizedResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"value":"`+strings.Repeat("a", maxResponseBytes)+`"}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.do(context.Background(), http.MethodGet, sessionsEndpoint, nil); err == nil {
		t.Fatalf("resposta acima de %d bytes foi aceita", maxResponseBytes)
	}
}

func TestJulesClientAcceptsBoundedLargeActivity(t *testing.T) {
	activityBytes := (2 << 20) + 1024
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"activities":[{"agentMessaged":{"agentMessage":"`+
			strings.Repeat("a", activityBytes)+`"}}]}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.do(
		context.Background(),
		http.MethodGet,
		"/sessions/large/activities?pageSize=1",
		nil,
	); err != nil {
		t.Fatalf("bounded Jules activity was rejected: %v", err)
	}
}

func TestManifestBatchLimits(t *testing.T) {
	t.Setenv("JULES_MAX_BATCH_SESSIONS", "3")
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	sessions := []map[string]string{
		{"title": "one", "prompt": "one"},
		{"title": "two", "prompt": "two"},
		{"title": "three", "prompt": "three"},
		{"title": "four", "prompt": "four"},
	}
	manifest := map[string]any{
		"source":              allowedSourceName,
		"startingBranch":      defaultStartingBranch,
		"requirePlanApproval": true,
		"automationMode":      unspecifiedAutomation,
		"commonPrompt":        "rules",
		"sessions":            sessions,
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("manifesto acima do lote configurado foi aceito")
	}
	t.Setenv("JULES_MAX_BATCH_SESSIONS", "4")
	if _, err := loadManifest(path); err != nil {
		t.Fatalf("manifesto no limite configurado foi rejeitado: %v", err)
	}
}

func TestCleanupVerifiesOnlySelectedSessions(t *testing.T) {
	var deleted bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/sessions/smoke":
			deleted = true
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == sessionsEndpoint:
			_, _ = io.WriteString(w, `{"sessions":[{"id":"unrelated","state":"IN_PROGRESS"}]}`)
		default:
			t.Fatalf("request inesperado: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	artifact := batchArtifact{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Sessions: []sessionRecord{{ID: "smoke"}}}
	if err := writeArtifact(path, artifact); err != nil {
		t.Fatal(err)
	}
	if err := runBatch(context.Background(), client, []string{"cleanup", "--inventory", path, "--confirm-count", "1"}); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("sessão selecionada não foi excluída")
	}
}

func TestMeasureLatestPlanUsesLatestRevision(t *testing.T) {
	activities := json.RawMessage(`{
		"activities":[
			{"planGenerated":{"plan":{"steps":[{"title":"generic plan"}]}}},
			{"planGenerated":{"plan":{"steps":[{"title":"AGENTS.md origin/main files invariants counterfactual RED coverage 80% PostgreSQL rollback sem PR"}]}}}
		]
	}`)
	metric, err := measureLatestPlan(sessionRecord{ID: "123", Title: "test", State: "AWAITING_PLAN_APPROVAL"}, activities)
	if err != nil {
		t.Fatal(err)
	}
	if metric.PlanCount != 2 || metric.Score != 10 || len(metric.Missing) != 0 {
		t.Fatalf("metric = %#v", metric)
	}
	if !metric.EligibleForReview {
		t.Fatal("plano completo e sem hard gate não ficou elegível")
	}
}

func TestBatchMeasurePaginatesActivitiesInBoundedGroups(t *testing.T) {
	var activityRequests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions/123":
			_, _ = io.WriteString(w, `{"id":"123","title":"test","state":"AWAITING_PLAN_APPROVAL"}`)
		case "/sessions/123/activities":
			activityRequests = append(activityRequests, r.URL.RawQuery)
			switch r.URL.Query().Get("pageToken") {
			case "":
				_, _ = io.WriteString(w, `{"activities":[{"planGenerated":{"plan":{"steps":[{"title":"generic plan"}]}}}],"nextPageToken":"page-2"}`)
			case "page-2":
				_, _ = io.WriteString(w, `{"activities":[{"planGenerated":{"plan":{"steps":[{"title":"AGENTS.md origin/main files invariants counterfactual RED coverage 80% PostgreSQL rollback sem PR"}]}}}],"nextPageToken":"page-3"}`)
			case "page-3":
				_, _ = io.WriteString(w, `{"activities":[{"planApproved":{}}]}`)
			default:
				t.Fatalf("pageToken inesperado: %q", r.URL.Query().Get("pageToken"))
			}
		default:
			t.Fatalf("rota inesperada: %s", r.URL.RequestURI())
		}
	}))
	defer upstream.Close()

	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	launchPath := filepath.Join(dir, "launch.json")
	outputPath := filepath.Join(dir, "metrics.json")
	if err := writeArtifact(launchPath, batchArtifact{Sessions: []sessionRecord{{ID: "123"}}}); err != nil {
		t.Fatal(err)
	}
	if err := runMeasure(context.Background(), client, []string{"--launch", launchPath, "--output", outputPath}); err != nil {
		t.Fatal(err)
	}
	baseQuery := "pageSize=10&fields=" + urlQueryEscape(approvalActivitySignalFields)
	wantRequests := []string{
		baseQuery,
		baseQuery + "&pageToken=page-2",
		baseQuery + "&pageToken=page-3",
	}
	if !reflect.DeepEqual(activityRequests, wantRequests) {
		t.Fatalf("activity requests = %#v, want %#v", activityRequests, wantRequests)
	}
	var result struct {
		GeneratedAt string       `json:"generatedAt"`
		Metrics     []planMetric `json:"metrics"`
	}
	if err := readJSONFile(outputPath, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Metrics) != 1 || result.Metrics[0].PlanCount != 2 || result.Metrics[0].PlanApprovalCount != 1 || result.Metrics[0].Score != 10 {
		t.Fatalf("metrics = %#v", result.Metrics)
	}
}

func TestListAllActivitiesFallsBackWhenGroupedPageIsTooLarge(t *testing.T) {
	var pageSizes []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageSize := r.URL.Query().Get("pageSize")
		pageSizes = append(pageSizes, pageSize)
		if pageSize == "10" {
			_, _ = io.WriteString(w, `{"activities":[{"value":"`+
				strings.Repeat("a", maxResponseBytes)+`"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"activities":[{"agentMessaged":{"agentMessage":"bounded"}}]}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	activities, err := listAllActivities(context.Background(), client, "large")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pageSizes, []string{"10", "1"}) {
		t.Fatalf("page sizes = %#v, want grouped attempt followed by safe fallback", pageSizes)
	}
	if !strings.Contains(string(activities), "bounded") {
		t.Fatalf("activities = %s", activities)
	}
}

func TestListAllActivitiesAcceptsLargeSingleActivityWithinDedicatedLimit(t *testing.T) {
	largeActivityBytes := maxResponseBytes + 1024
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageSize") == "10" {
			_, _ = io.WriteString(w, `{"activities":[{"value":"`+
				strings.Repeat("a", maxResponseBytes)+`"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"activities":[{"agentMessaged":{"agentMessage":"`+
			strings.Repeat("b", largeActivityBytes)+`"}}]}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	activities, err := listAllActivities(context.Background(), client, "large-single")
	if err != nil {
		t.Fatalf("large single activity was rejected: %v", err)
	}
	if !strings.Contains(string(activities), strings.Repeat("b", 128)) {
		t.Fatal("large activity payload was not preserved")
	}
}

func TestListAllPlanSignalsExcludeCodeAndMessages(t *testing.T) {
	var fields string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields = r.URL.Query().Get("fields")
		_, _ = io.WriteString(w, `{"activities":[{"planApproved":{}}]}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	activities, err := listAllPlanSignals(context.Background(), client, "projected")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"nextPageToken", "planGenerated.plan.steps", "planApproved",
	} {
		if !strings.Contains(fields, required) {
			t.Fatalf("fields = %q, missing %q", fields, required)
		}
	}
	for _, forbidden := range []string{"agentMessaged", "userMessaged", "changeSet"} {
		if strings.Contains(fields, forbidden) {
			t.Fatalf("fields = %q, contains forbidden payload %q", fields, forbidden)
		}
	}
	if !strings.Contains(string(activities), "planApproved") {
		t.Fatalf("activities = %s", activities)
	}
}

func TestListAllReconcileSignalsExcludePlansAndCode(t *testing.T) {
	var fields string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields = r.URL.Query().Get("fields")
		_, _ = io.WriteString(w, `{"activities":[]}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listAllReconcileSignals(context.Background(), client, "projected"); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"nextPageToken", "createTime", "agentMessaged.agentMessage",
		"userMessaged.userMessage",
	} {
		if !strings.Contains(fields, required) {
			t.Fatalf("fields = %q, missing %q", fields, required)
		}
	}
	for _, forbidden := range []string{"planGenerated", "changeSet"} {
		if strings.Contains(fields, forbidden) {
			t.Fatalf("fields = %q, contains forbidden payload %q", fields, forbidden)
		}
	}
}

func TestBatchInventoryAndStatusPersistObservedSessions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case sessionsEndpoint:
			_, _ = io.WriteString(w, `{"sessions":[{"id":"session-1","title":"queued","state":"QUEUED","createTime":"2026-08-02T09:00:00Z"}]}`)
		case "/sessions/session-1":
			_, _ = io.WriteString(w, `{"id":"session-1","title":"done","state":"COMPLETED","createTime":"2026-08-02T09:00:00Z","updateTime":"2026-08-02T09:01:00Z"}`)
		default:
			t.Fatalf("unexpected request: %s", r.URL.RequestURI())
		}
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	inventoryPath := filepath.Join(dir, "inventory.json")
	statusPath := filepath.Join(dir, "status.json")
	if err := runBatch(context.Background(), client, []string{
		"inventory", "--output", inventoryPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runBatch(context.Background(), client, []string{
		"status", "--launch", inventoryPath, "--output", statusPath,
	}); err != nil {
		t.Fatal(err)
	}

	artifact, err := loadArtifact(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Sessions) != 1 || artifact.Sessions[0].State != "COMPLETED" ||
		artifact.Sessions[0].Title != "done" {
		t.Fatalf("status artifact = %#v", artifact)
	}
}

func TestListAllSessionsUsesBoundedFieldProjection(t *testing.T) {
	const expectedFields = "nextPageToken,sessions(id,name,prompt,title,state,createTime,updateTime,outputs.pullRequest.url)"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Query().Get("fields") != expectedFields {
			_, _ = io.WriteString(w, `{"sessions":[{"oversized":"`+
				strings.Repeat("x", maxResponseBytes)+`"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"sessions":[{
				"id":"session-large",
				"prompt":"ISSUE: #337",
				"title":"bounded",
				"state":"COMPLETED",
				"outputs":[{"pullRequest":{"url":"https://github.com/emersonbusson/ramshared/pull/1700"}}]
			}]
		}`)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	sessions, err := listAllSessions(context.Background(), client)
	if err != nil {
		t.Fatalf("bounded session inventory failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "session-large" ||
		sessionPullRequestURL(sessions[0].Raw) != "https://github.com/emersonbusson/ramshared/pull/1700" ||
		!strings.Contains(string(sessions[0].Raw), "ISSUE: #337") {
		t.Fatalf("projected sessions lost required fields: %#v", sessions)
	}
}

func TestListAllSessionsRejectsRepeatedPageToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(
			w,
			`{"sessions":[],"nextPageToken":"repeated"}`,
		)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = listAllSessions(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "repetiu pageToken") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyInventoriesUseArraysInsteadOfNull(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case sessionsEndpoint:
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case "/sessions/session/activities":
			_, _ = io.WriteString(w, `{"activities":[]}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.RequestURI())
		}
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := listAllSessions(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if sessions == nil || len(sessions) != 0 {
		t.Fatalf("sessions = %#v, want non-nil empty slice", sessions)
	}
	activities, err := listAllActivities(
		context.Background(),
		client,
		"session",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(activities), `"activities":[]`) {
		t.Fatalf("activities = %s", activities)
	}
}

func TestListAllActivitiesBoundsUniquePagination(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprintf(
			w,
			`{"activities":[],"nextPageToken":"page-%d"}`,
			requests,
		)
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = listAllActivities(context.Background(), client, "session")
	if err == nil || !strings.Contains(err.Error(), "limite de páginas") {
		t.Fatalf("err = %v", err)
	}
	if requests != maxActivityPages {
		t.Fatalf("requests = %d, want %d", requests, maxActivityPages)
	}
}

func TestMeasureBlocksChangeSetWithoutApproval(t *testing.T) {
	activities := json.RawMessage(`{
		"activities":[
			{"planGenerated":{"plan":{"steps":[{"title":"AGENTS.md origin/main files invariants counterfactual RED coverage 80% PostgreSQL rollback sem PR"}]}}},
			{"artifacts":[{"changeSet":{"gitPatch":{"unidiffPatch":"diff"}}}]}
		]
	}`)
	metric, err := measureLatestPlan(sessionRecord{ID: "123"}, activities)
	if err != nil {
		t.Fatal(err)
	}
	if metric.EligibleForReview || metric.ChangeSetCount != 1 || len(metric.HardGates) != 1 {
		t.Fatalf("metric = %#v", metric)
	}
}

func TestMCPMeasurePlanIsReadOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("measure fez mutação: %s", r.Method)
		}
		switch r.URL.Path {
		case "/sessions/123":
			_, _ = io.WriteString(w, `{"id":"123","title":"test","state":"AWAITING_PLAN_APPROVAL"}`)
		case "/sessions/123/activities":
			_, _ = io.WriteString(w, `{"activities":[{"planGenerated":{"plan":{"steps":[{"title":"AGENTS.md origin/main files invariants counterfactual RED coverage 80% PostgreSQL rollback sem PR"}]}}}]}`)
		default:
			t.Fatalf("rota inesperada: %s", r.URL.RequestURI())
		}
	}))
	defer upstream.Close()
	client, err := newJulesClient("key", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpJulesServer{client: client, allowedSource: allowedSourceName, logger: nilLogger()}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"sessionId": "123"}}}
	result, err := s.measurePlan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"score":10`) {
		t.Fatalf("resultado não contém score 10: %s", encoded)
	}
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
