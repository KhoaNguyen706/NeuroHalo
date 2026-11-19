// The mcp service lets an AI agent (Claude Code, Claude Desktop or any MCP
// client) ask NeuroHalo about its sessions: what is recording now, how a
// session went, and its written summary. It is a thin client of the api's
// REST endpoints, so it has no database access of its own and the api stays
// the one place that knows the rules. It speaks MCP over streamable HTTP at
// http://localhost:8090/mcp.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// client calls the api with the bearer token.
// Created by: main. Used by: every tool handler.
type client struct {
	base  string
	token string
	http  *http.Client
}

// call sends one request to the api and returns its body. An answer other
// than 2xx becomes an error carrying the api's own message, which the tool
// hands to the agent.
func (c *client) call(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the NeuroHalo api is not reachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("the api answered %s", resp.Status)
	}
	return body, nil
}

// text wraps a tool's answer as one text block.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// session is the part of the api's session JSON the tools show.
// Read by: listSessions.
type session struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Source      string     `json:"source"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt"`
	Participant string     `json:"participant"`
	Task        string     `json:"task"`
}

// noInput is the input of a tool that takes none.
type noInput struct{}

// sessionInput names one session.
type sessionInput struct {
	SessionID string `json:"session_id" jsonschema:"the session's id, as list_sessions gives it, e.g. s-464b23dd8f77"`
}

// eventsInput names one session and, optionally, one kind of event.
type eventsInput struct {
	SessionID string `json:"session_id" jsonschema:"the session's id, as list_sessions gives it"`
	Type      string `json:"type,omitempty" jsonschema:"only this kind: channel_bad, channel_recovered, marker, eye_movement, muscle or alpha_rise; empty for all"`
}

// summaryInput names one session and whether to write the summary again.
type summaryInput struct {
	SessionID string `json:"session_id" jsonschema:"the session's id, as list_sessions gives it"`
	Fresh     bool   `json:"fresh,omitempty" jsonschema:"write a new summary even if one is stored"`
}

// newServer builds the MCP server with its tools.
// Used by: main.
func newServer(c *client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "neurohalo", Version: "1.0.0"}, &mcp.ServerOptions{
		Instructions: "NeuroHalo records EEG sessions live and checks their data quality. " +
			"Use list_sessions or live_overview to find a session, then session_facts for what was measured " +
			"(bad sensors with times, eye and muscle activity, markers, ERP trials and SME). " +
			"session_summary asks Claude for a short written verdict and costs an API call; prefer session_facts " +
			"when you can reason from the facts yourself. This is research data quality, not medical advice.",
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "The newest 100 sessions, newest first: id, name, live or ended, start time, length, participant and task.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		body, err := c.call(ctx, http.MethodGet, "/sessions")
		if err != nil {
			return nil, nil, err
		}
		var list []session
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, nil, err
		}
		if len(list) == 0 {
			return text("No sessions yet."), nil, nil
		}
		var b strings.Builder
		for _, x := range list {
			name := x.Label
			if name == "" {
				name = "(" + x.Source + ")"
			}
			length := "still recording"
			if x.EndedAt != nil {
				length = x.EndedAt.Sub(x.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(&b, "%s  %s  %s  started %s  %s", x.ID, name, x.Status, x.StartedAt.Format(time.RFC3339), length)
			if x.Participant != "" {
				fmt.Fprintf(&b, "  participant %s, task %s", x.Participant, x.Task)
			}
			b.WriteString("\n")
		}
		return text(b.String()), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "live_overview",
		Description: "Every session being recorded right now: seconds so far, whether data is still arriving, " +
			"sensors bad now, trials, eye movements and muscle activity. JSON.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		body, err := c.call(ctx, http.MethodGet, "/live")
		if err != nil {
			return nil, nil, err
		}
		return text(string(body)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_facts",
		Description: "What NeuroHalo measured in one session, as plain text: length, each stretch a sensor was bad " +
			"(flat or noisy) with times, sensors still bad, eye movements and muscle activity per minute, markers " +
			"per label, and per label how many ERP trials are kept at 100 and 200 uV and the SME (how precise the " +
			"average is, in uV). Free and instant.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, any, error) {
		body, err := c.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(in.SessionID)+"/facts")
		if err != nil {
			return nil, nil, err
		}
		var f struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &f); err != nil {
			return nil, nil, err
		}
		return text(f.Text), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_events",
		Description: "One session's events in time order, as JSON: type, channel, time and details.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, any, error) {
		body, err := c.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(in.SessionID)+"/events")
		if err != nil {
			return nil, nil, err
		}
		if in.Type == "" {
			return text(string(body)), nil, nil
		}
		var all []map[string]any
		if err := json.Unmarshal(body, &all); err != nil {
			return nil, nil, err
		}
		kept := []map[string]any{}
		for _, e := range all {
			if e["type"] == in.Type {
				kept = append(kept, e)
			}
		}
		out, _ := json.Marshal(kept)
		return text(string(out)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_summary",
		Description: "A short written verdict on one session (is the recording usable, what went wrong, what to do " +
			"next), written by Claude from session_facts. A stored summary is returned at no cost; otherwise this " +
			"makes one Claude API call. Fails if the api has no ANTHROPIC_API_KEY.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in summaryInput) (*mcp.CallToolResult, any, error) {
		path := "/sessions/" + url.PathEscape(in.SessionID) + "/summary"
		if in.Fresh {
			path += "?fresh=1"
		}
		body, err := c.call(ctx, http.MethodPost, path)
		if err != nil {
			return nil, nil, err
		}
		var sum struct {
			Body  string `json:"body"`
			Model string `json:"model"`
			Final bool   `json:"final"`
		}
		if err := json.Unmarshal(body, &sum); err != nil {
			return nil, nil, err
		}
		note := "written by " + sum.Model
		if !sum.Final {
			note += " while the session was still recording, so it is not stored"
		}
		return text(sum.Body + "\n\n(" + note + ")"), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_studies",
		Description: "Every study (a group of sessions exported together as a BIDS dataset) with its session count. JSON.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		body, err := c.call(ctx, http.MethodGet, "/studies")
		if err != nil {
			return nil, nil, err
		}
		return text(string(body)), nil, nil
	})

	return s
}

// main serves MCP over streamable HTTP on LISTEN_ADDR (default :8090) at /mcp.
func main() {
	token := os.Getenv("API_TOKEN")
	if token == "" {
		log.Fatal("API_TOKEN is not set")
	}
	c := &client{
		base:  strings.TrimRight(env("API_URL", "http://localhost:8080"), "/"),
		token: token,
		http:  &http.Client{Timeout: 3 * time.Minute}, // a summary can take a while
	}
	server := newServer(c)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	addr := env("LISTEN_ADDR", ":8090")
	log.Printf("mcp listening on %s/mcp, api at %s", addr, c.base)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
