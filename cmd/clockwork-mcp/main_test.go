package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/httpapi"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// When re-executed by the test with this variable set, the test binary becomes
// clockwork-mcp itself, so the test drives the real program over stdio.
func TestMain(m *testing.M) {
	if os.Getenv("CLOCKWORK_MCP_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const adminToken = "mcp-e2e-admin-token-0123456789"

func startClockwork(t *testing.T) (url, appKey string) {
	t.Helper()
	repo := tasks.NewMemoryRepository()
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Tasks:      tasks.NewService(repo, tasks.Options{}),
		Executions: executions.NewService(executions.NewMemoryRepository(repo.Get), nil),
		Apps:       apps.NewService(apps.NewMemoryRepository(), nil),
		AdminToken: adminToken,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	post := func(path, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	app := post("/applications", `{"name":"stdio-app"}`)
	repo.AddApplication(app["id"].(string))
	key := post("/applications/"+app["id"].(string)+"/keys", `{"name":"e2e"}`)
	return srv.URL, key["key"].(string)
}

func command(url, key string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CLOCKWORK_MCP_RUN_MAIN=1", "CLOCKWORK_URL="+url, "CLOCKWORK_API_KEY="+key)
	return cmd
}

// The M8 exit criterion: a client over stdio creates a task, runs it and reads the result.
func TestStdioEndToEnd(t *testing.T) {
	url, key := startClockwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: command(url, key)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if init := cs.InitializeResult(); init.ServerInfo.Name != "clockwork" || !strings.Contains(init.ServerInfo.Title, "stdio-app") ||
		!strings.Contains(init.Instructions, "output_untrusted") {
		t.Fatalf("server info: %+v / %q", init.ServerInfo, init.Instructions)
	}
	text := func(name string, args map[string]any) map[string]any {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	task := text("create_task", map[string]any{"name": "over-stdio", "url": "https://example.com/ping"})
	run := text("run_task", map[string]any{"task_id": task["id"]})
	exe := text("get_run", map[string]any{"execution_id": run["execution"].(map[string]any)["id"]})
	if exe["task_name"] != "over-stdio" || exe["trigger"] != "manual" {
		t.Fatalf("run read back over stdio: %v", exe)
	}
}

func TestRefusesAdminTokenAtStartup(t *testing.T) {
	url, _ := startClockwork(t)
	out, err := command(url, adminToken).CombinedOutput()
	if err == nil {
		t.Fatal("started with the admin token")
	}
	if !strings.Contains(string(out), "is the admin token") {
		t.Fatalf("unexpected output: %s", out)
	}
}
