// Package agentclient talks to the Python agent service.
//
// Contract in docs/agent-api.md. The client is deliberately thin: the runner
// hands over an alert and receives a diagnosis, and the agent's tool calls happen
// entirely on its own side. Nothing else passes between them, which is what makes
// it impossible for the runner to leak anything to the agent beyond the alert.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Observed is the measurement behind an alert.
type Observed struct {
	Signal   string  `json:"signal"`
	Value    float64 `json:"value"`
	Baseline float64 `json:"baseline"`
	Unit     string  `json:"unit"`
}

// Alert is what the agent is told.
//
// Service is where the alarm fired -- the symptom -- which for most scenarios is
// not the root cause. Summary is generated from measured metrics rather than
// authored, so it cannot carry a hint the metrics do not.
type Alert struct {
	Service  string   `json:"service"`
	Summary  string   `json:"summary"`
	FiredAt  string   `json:"fired_at"`
	Observed Observed `json:"observed"`
}

type Budget struct {
	MaxSteps        int `json:"max_steps"`
	DeadlineSeconds int `json:"deadline_seconds"`
}

type Request struct {
	EpisodeID string `json:"episode_id"`
	Alert     Alert  `json:"alert"`
	Budget    Budget `json:"budget"`
}

// Response mirrors scoring.Answer, plus the agent's own timing.
type Response struct {
	RootCauseService string   `json:"root_cause_service"`
	RootCauseClass   string   `json:"root_cause_class"`
	Confidence       float64  `json:"confidence"`
	Healthy          bool     `json:"healthy"`
	Escalate         bool     `json:"escalate"`
	Steps            int      `json:"steps"`
	ToolCalls        []string `json:"tool_calls"`
	Reasoning        string   `json:"reasoning"`
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	ElapsedSeconds   float64  `json:"elapsed_seconds"`
}

type Health struct {
	OK       bool     `json:"ok"`
	Brain    string   `json:"brain"`
	Tools    int      `json:"tools"`
	Withheld []string `json:"withheld"`
	Error    string   `json:"error"`
}

type Client struct {
	base string
	http *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

// Health polls the agent.
//
// Called before a run starts so a misconfigured agent fails in the first second
// rather than forty minutes in, and so the arm's identity is recorded from the
// agent itself rather than from a command-line label that might not match.
func (c *Client) Health(ctx context.Context) (Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("agent unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	var out Health
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Health{}, fmt.Errorf("decode health response: %w", err)
	}
	if !out.OK {
		return out, fmt.Errorf("agent reports not ready: %s", out.Error)
	}
	return out, nil
}

// Diagnose asks the agent to diagnose one episode.
//
// An error here means the episode cannot be scored, not that the agent was wrong.
// The runner marks such episodes invalid and excludes them from rates.
func (c *Client) Diagnose(ctx context.Context, r Request) (Response, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.base+"/diagnose", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("diagnose call failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read diagnose response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The agent reports its own failures as JSON, so surface that rather
		// than a bare status code -- "the brain raised KeyError" is worth
		// knowing when reading a run's discard list.
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
			return Response{}, fmt.Errorf("agent error: %s", errBody.Error)
		}
		return Response{}, fmt.Errorf("agent returned HTTP %d: %s",
			resp.StatusCode, truncate(string(raw), 200))
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("decode diagnose response: %w", err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
