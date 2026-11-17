// Command agent runs on the machine to be controlled. It connects out to the
// relay and executes screenshot / input / window / command requests.
//
// Configuration (first match wins): flags, environment variables
// RMCP_SERVER / RMCP_TOKEN / RMCP_NAME, or agent.json next to the executable:
//
//	{"server": "https://relay.example.com", "token": "…", "name": "lab-pc"}
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/krantro2938/remote-mcp/internal/agent"
	"github.com/krantro2938/remote-mcp/internal/proto"
)

type fileConfig struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
}

func main() {
	var (
		server   = flag.String("server", "", "relay URL, e.g. https://relay.example.com")
		token    = flag.String("token", "", "agent token (one of the relay's AGENT_TOKENS)")
		name     = flag.String("name", "", "display name for this machine (default: hostname)")
		readOnly = flag.Bool("read-only", false, "only allow screenshots and listing windows/files")
		demo     = flag.Bool("demo", false, "use the fake demo desktop instead of the real screen")
		config   = flag.String("config", "", "path to agent.json (default: next to the executable)")
	)
	flag.Parse()
	log.SetFlags(log.Ltime)

	var fc fileConfig
	cfgPath := *config
	if cfgPath == "" {
		if exe, err := os.Executable(); err == nil {
			cfgPath = filepath.Join(filepath.Dir(exe), "agent.json")
		}
	}
	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(data, &fc); err != nil {
			fatal("cannot parse %s: %v", cfgPath, err)
		}
	}
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	cfg := agent.Config{
		Server:   pick(*server, os.Getenv("RMCP_SERVER"), fc.Server),
		Token:    pick(*token, os.Getenv("RMCP_TOKEN"), fc.Token),
		Name:     pick(*name, os.Getenv("RMCP_NAME"), fc.Name),
		ReadOnly: *readOnly || fc.ReadOnly,
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	if cfg.Server == "" || cfg.Token == "" {
		fatal("missing server or token.\n\nEither run:\n  agent.exe -server https://your-relay -token YOUR_TOKEN\nor put agent.json next to agent.exe:\n  {\"server\": \"https://your-relay\", \"token\": \"YOUR_TOKEN\"}")
	}
	cfg.Server = agent.NormalizeServer(cfg.Server)

	var backend agent.Backend
	if *demo {
		backend = agent.NewDemoBackend()
	} else {
		backend = agent.NewPlatformBackend()
	}

	agent.SetConsoleTitle("remote-mcp agent - REMOTE CONTROL ACTIVE - close this window to stop")
	mode := "full control"
	if cfg.ReadOnly {
		mode = "read-only"
	}
	fmt.Printf(`
  ==============================================================
   remote-mcp agent %s  (%s, %s backend)
   This computer can be viewed and controlled remotely via
   %s
   Every action is logged below. Close this window to stop.
  ==============================================================

`, proto.Version, mode, backend.Name(), cfg.Server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := agent.New(cfg, backend).Run(ctx); err != nil && ctx.Err() == nil {
		fatal("%v", err)
	}
	log.Println("stopped")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	// When started by double-click the console closes immediately; keep it open.
	if agent.StartedFromExplorer() {
		fmt.Fprint(os.Stderr, "\nPress Enter to exit...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}
