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

type snapshotCreateResponse struct {
	SnapshotID string `json:"snapshot_id"`
	Error      string `json:"error"`
}

type snapshotRestoreResponse struct {
	SandboxID string `json:"sandbox_id"`
	Error     string `json:"error"`
}

type runResult struct {
	RestoreOnly time.Duration
	RestoreTTI  time.Duration
}

func main() {
	endpoint := flag.String("endpoint", "http://localhost:8080", "server base URL")
	iterations := flag.Int("iterations", 50, "measured iterations")
	warmup := flag.Int("warmup", 5, "warmup iterations (not recorded)")
	mutationCmd := flag.String("mutation-cmd", `sh -lc 'mkdir -p /opt/manta && echo "restored-ok" > /opt/manta/state.txt'`, "deterministic mutation workload command")
	sanityCmd := flag.String("sanity-cmd", `cat /opt/manta/state.txt`, "sanity command run after restore")
	expectStdout := flag.String("expect-stdout", "restored-ok\n", "expected stdout for sanity command")
	timeout := flag.Duration("timeout", 90*time.Second, "http request timeout")
	flag.Parse()

	client := &http.Client{Timeout: *timeout}
	base := strings.TrimRight(*endpoint, "/")
	fmt.Fprintf(os.Stderr, "Benchmark config: endpoint=%s warmup=%d iterations=%d mutation_cmd=%q sanity_cmd=%q\n", base, *warmup, *iterations, *mutationCmd, *sanityCmd)

	snapshotID, err := prepareFixture(client, base, *mutationCmd, *sanityCmd, *expectStdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture setup failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "fixture snapshot_id=%s\n", snapshotID)

	for i := 0; i < *warmup; i++ {
		if _, _, err := runOnce(client, base, snapshotID, *sanityCmd, *expectStdout); err != nil {
			fmt.Fprintf(os.Stderr, "warmup [%d/%d] failed: %v\n", i+1, *warmup, err)
		}
	}

	results := make([]runResult, 0, *iterations)
	failures := map[string]int{
		"restore_api": 0,
		"sanity_exec": 0,
		"destroy":     0,
		"other":       0,
	}
	for i := 0; i < *iterations; i++ {
		res, class, err := runOnce(client, base, snapshotID, *sanityCmd, *expectStdout)
		if err != nil {
			failures[class]++
			fmt.Fprintf(os.Stderr, "run [%d/%d] failed (%s): %v\n", i+1, *iterations, class, err)
			continue
		}
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "run [%d/%d] restore_only=%s restore_tti=%s\n", i+1, *iterations, res.RestoreOnly, res.RestoreTTI)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no successful runs")
		os.Exit(1)
	}

	restoreOnly := make([]time.Duration, 0, len(results))
	restoreTTI := make([]time.Duration, 0, len(results))
	for _, r := range results {
		restoreOnly = append(restoreOnly, r.RestoreOnly)
		restoreTTI = append(restoreTTI, r.RestoreTTI)
	}
	sort.Slice(restoreOnly, func(i, j int) bool { return restoreOnly[i] < restoreOnly[j] })
	sort.Slice(restoreTTI, func(i, j int) bool { return restoreTTI[i] < restoreTTI[j] })

	summary := map[string]any{
		"snapshot_id":           snapshotID,
		"iterations_requested":  *iterations,
		"iterations_success":    len(results),
		"failures":              failures,
		"restore_only_min_ns":   restoreOnly[0].Nanoseconds(),
		"restore_only_p50_ns":   percentile(restoreOnly, 0.50).Nanoseconds(),
		"restore_only_p95_ns":   percentile(restoreOnly, 0.95).Nanoseconds(),
		"restore_only_p99_ns":   percentile(restoreOnly, 0.99).Nanoseconds(),
		"restore_only_max_ns":   restoreOnly[len(restoreOnly)-1].Nanoseconds(),
		"restore_tti_min_ns":    restoreTTI[0].Nanoseconds(),
		"restore_tti_p50_ns":    percentile(restoreTTI, 0.50).Nanoseconds(),
		"restore_tti_p95_ns":    percentile(restoreTTI, 0.95).Nanoseconds(),
		"restore_tti_p99_ns":    percentile(restoreTTI, 0.99).Nanoseconds(),
		"restore_tti_max_ns":    restoreTTI[len(restoreTTI)-1].Nanoseconds(),
		"sanity_cmd":            *sanityCmd,
		"sanity_expected_stdout": *expectStdout,
	}

	fmt.Fprintf(os.Stderr, "\n--- Restore-only (%d successful runs) ---\n", len(results))
	fmt.Fprintf(os.Stderr, "min: %s\n", restoreOnly[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(restoreOnly, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(restoreOnly, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(restoreOnly, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", restoreOnly[len(restoreOnly)-1])
	fmt.Fprintf(os.Stderr, "\n--- Restore TTI (restore + first exec) ---\n")
	fmt.Fprintf(os.Stderr, "min: %s\n", restoreTTI[0])
	fmt.Fprintf(os.Stderr, "p50: %s\n", percentile(restoreTTI, 0.50))
	fmt.Fprintf(os.Stderr, "p95: %s\n", percentile(restoreTTI, 0.95))
	fmt.Fprintf(os.Stderr, "p99: %s\n", percentile(restoreTTI, 0.99))
	fmt.Fprintf(os.Stderr, "max: %s\n", restoreTTI[len(restoreTTI)-1])

	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "encode summary: %v\n", err)
		os.Exit(1)
	}
}

func prepareFixture(client *http.Client, endpoint, mutationCmd, sanityCmd, expectStdout string) (string, error) {
	sbID, err := createSandbox(client, endpoint)
	if err != nil {
		return "", fmt.Errorf("create fixture sandbox: %w", err)
	}
	defer func() {
		_ = destroySandbox(client, endpoint, sbID)
	}()

	if _, err := execInSandbox(client, endpoint, sbID, mutationCmd); err != nil {
		return "", fmt.Errorf("run mutation workload: %w", err)
	}
	sanity, err := execInSandbox(client, endpoint, sbID, sanityCmd)
	if err != nil {
		return "", fmt.Errorf("sanity validation failed: %w", err)
	}
	if sanity != expectStdout {
		return "", fmt.Errorf("sanity stdout mismatch during fixture setup: got=%q want=%q", sanity, expectStdout)
	}
	snapshotID, err := createUserSnapshot(client, endpoint, sbID)
	if err != nil {
		return "", fmt.Errorf("create user snapshot: %w", err)
	}
	return snapshotID, nil
}

func runOnce(client *http.Client, endpoint, snapshotID, sanityCmd, expectStdout string) (runResult, string, error) {
	restoreStart := time.Now()
	sbID, err := restoreSandbox(client, endpoint, snapshotID)
	if err != nil {
		return runResult{}, "restore_api", fmt.Errorf("restore request: %w", err)
	}
	restoreOnly := time.Since(restoreStart)

	stdout, err := execInSandbox(client, endpoint, sbID, sanityCmd)
	if err != nil {
		_ = destroySandbox(client, endpoint, sbID)
		return runResult{}, "sanity_exec", fmt.Errorf("sanity exec: %w", err)
	}
	if stdout != expectStdout {
		_ = destroySandbox(client, endpoint, sbID)
		return runResult{}, "sanity_exec", fmt.Errorf("sanity stdout mismatch: got=%q want=%q", stdout, expectStdout)
	}
	restoreTTI := time.Since(restoreStart)

	if err := destroySandbox(client, endpoint, sbID); err != nil {
		return runResult{}, "destroy", fmt.Errorf("destroy failed: %w", err)
	}
	return runResult{RestoreOnly: restoreOnly, RestoreTTI: restoreTTI}, "", nil
}

func createSandbox(client *http.Client, endpoint string) (string, error) {
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/create", http.NoBody)
	raw, err := doJSON(client, req)
	if err != nil {
		return "", err
	}
	var resp createResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode create response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" || strings.TrimSpace(resp.SandboxID) == "" {
		return "", fmt.Errorf("create failed: error=%q sandbox_id=%q body=%q", strings.TrimSpace(resp.Error), resp.SandboxID, strings.TrimSpace(string(raw)))
	}
	return resp.SandboxID, nil
}

func createUserSnapshot(client *http.Client, endpoint, sandboxID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"sandbox_id": sandboxID, "name": "bench-restore-fixture"})
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/snapshot/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return "", err
	}
	var resp snapshotCreateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode snapshot/create response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" || strings.TrimSpace(resp.SnapshotID) == "" {
		return "", fmt.Errorf("snapshot/create failed: error=%q snapshot_id=%q body=%q", strings.TrimSpace(resp.Error), resp.SnapshotID, strings.TrimSpace(string(raw)))
	}
	return resp.SnapshotID, nil
}

func restoreSandbox(client *http.Client, endpoint, snapshotID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"snapshot_id": snapshotID})
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/snapshot/restore", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return "", err
	}
	var resp snapshotRestoreResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode snapshot/restore response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" || strings.TrimSpace(resp.SandboxID) == "" {
		return "", fmt.Errorf("snapshot/restore failed: error=%q sandbox_id=%q body=%q", strings.TrimSpace(resp.Error), resp.SandboxID, strings.TrimSpace(string(raw)))
	}
	return resp.SandboxID, nil
}

func execInSandbox(client *http.Client, endpoint, sandboxID, cmd string) (string, error) {
	body, _ := json.Marshal(map[string]string{"sandbox_id": sandboxID, "cmd": cmd})
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/exec", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	raw, err := doJSON(client, req)
	if err != nil {
		return "", err
	}
	var resp execResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode exec response: %w (body=%q)", err, strings.TrimSpace(string(raw)))
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("exec exit code %d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	return resp.Stdout, nil
}

func destroySandbox(client *http.Client, endpoint, sandboxID string) error {
	body, _ := json.Marshal(map[string]string{"sandbox_id": sandboxID})
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
