package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type createResponse struct {
	SandboxID string `json:"sandbox_id"`
	Error     string `json:"error"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

type destroyResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type runResult struct {
	Duration time.Duration
}

func main() {
	endpoint := flag.String("endpoint", "http://localhost:8080", "server base URL")
	iterations := flag.Int("iterations", 50, "measured iterations")
	warmup := flag.Int("warmup", 5, "warmup iterations (not recorded)")
	cmd := flag.String("cmd", `echo "benchmark"`, "command to run in sandbox")
	timeout := flag.Duration("timeout", 90*time.Second, "http request timeout")
	flag.Parse()

	client := &http.Client{Timeout: *timeout}
	base := strings.TrimRight(*endpoint, "/")

	fmt.Fprintf(os.Stderr, "Benchmark config: endpoint=%s warmup=%d iterations=%d cmd=%q\n", base, *warmup, *iterations, *cmd)

	for i := 0; i < *warmup; i++ {
		if _, err := runOnce(client, base, *cmd); err != nil {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] failed: %v\n", i+1, *warmup, err)
		}
	}

	results := make([]runResult, 0, *iterations)
	for i := 0; i < *iterations; i++ {
		res, err := runOnce(client, base, *cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run [%d/%d] failed: %v\n", i+1, *iterations, err)
			continue
		}
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "run [%d/%d] %s\n", i+1, *iterations, res.Duration)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no successful runs")
		os.Exit(1)
	}

	durations := make([]time.Duration, 0, len(results))
	for _, r := range results {
		durations = append(durations, r.Duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	summary := map[string]any{
		"iterations_requested": *iterations,
		"iterations_success":   len(durations),
		"min_ns":               durations[0].Nanoseconds(),
		"p50_ns":               percentile(durations, 0.50).Nanoseconds(),
		"p95_ns":               percentile(durations, 0.95).Nanoseconds(),
		"p99_ns":               percentile(durations, 0.99).Nanoseconds(),
		"max_ns":               durations[len(durations)-1].Nanoseconds(),
	}

	fmt.Fprintf(os.Stderr, "\n--- Results (%d successful runs) ---\n", len(durations))
	fmt.Fprintf(os.Stderr, "min: %s\n", durations[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(durations, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(durations, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(durations, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", durations[len(durations)-1])

	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "encode summary: %v\n", err)
		os.Exit(1)
	}
}

func runOnce(client *http.Client, endpoint, cmd string) (runResult, error) {
	start := time.Now()

	createReq, _ := http.NewRequest(http.MethodPost, endpoint+"/create", http.NoBody)
	createRespRaw, err := doJSON(client, createReq)
	if err != nil {
		return runResult{}, fmt.Errorf("create request: %w", err)
	}
	var createResp createResponse
	if err := json.Unmarshal(createRespRaw, &createResp); err != nil {
		return runResult{}, fmt.Errorf("decode create response: %w (body=%q)", err, strings.TrimSpace(string(createRespRaw)))
	}
	if createResp.Error != "" || createResp.SandboxID == "" {
		return runResult{}, fmt.Errorf("create failed: error=%q sandbox_id=%q body=%q", strings.TrimSpace(createResp.Error), createResp.SandboxID, strings.TrimSpace(string(createRespRaw)))
	}

	destroy := func() error {
		body, _ := json.Marshal(map[string]string{"sandbox_id": createResp.SandboxID})
		req, _ := http.NewRequest(http.MethodPost, endpoint+"/destroy", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		raw, err := doJSON(client, req)
		if err != nil {
			return err
		}
		var resp destroyResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode destroy response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		return nil
	}

	execBody, _ := json.Marshal(map[string]string{
		"sandbox_id": createResp.SandboxID,
		"cmd":        cmd,
	})
	execReq, _ := http.NewRequest(http.MethodPost, endpoint+"/exec", bytes.NewReader(execBody))
	execReq.Header.Set("Content-Type", "application/json")
	execRespRaw, err := doJSON(client, execReq)
	if err != nil {
		_ = destroy()
		return runResult{}, fmt.Errorf("exec request: %w", err)
	}
	var execResp execResponse
	if err := json.Unmarshal(execRespRaw, &execResp); err != nil {
		_ = destroy()
		return runResult{}, fmt.Errorf("decode exec response: %w", err)
	}
	if execResp.Error != "" {
		_ = destroy()
		return runResult{}, fmt.Errorf("exec failed: %s", execResp.Error)
	}

	elapsed := time.Since(start)

	if err := destroy(); err != nil {
		return runResult{}, fmt.Errorf("destroy failed: %w", err)
	}

	return runResult{Duration: elapsed}, nil
}

func doJSON(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d body=%s", resp.StatusCode, strings.TrimSpace(raw.String()))
	}
	return raw.Bytes(), nil
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}
