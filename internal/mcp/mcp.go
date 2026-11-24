// Package mcp serves berth to AI agents over the Model Context Protocol (stdio, JSON-RPC 2.0).
//
// Jobs an agent submits are bound to this server process: when the agent's
// session ends, its queued and running jobs are cancelled, unless it asked to
// keep them.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/units"
)

const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

var jobProps = map[string]any{
	"command":       str("Shell command line, run with /bin/sh -c. Use this or argv."),
	"argv":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command and arguments, no shell."},
	"cwd":           str("Absolute working directory. Default: the server's working directory."),
	"memory":        str("Memory to reserve and hard-cap, e.g. \"4G\". Default: the host's default."),
	"cpus":          num("CPU cores to reserve and cap. Default: the host's default."),
	"timeout":       str("Execution deadline, e.g. \"20m\". Default: none."),
	"queue_timeout": str("Give up if not started within this long, e.g. \"10m\". Default: none."),
	"gpus":          num("GPUs to reserve (pool resource \"gpu\")."),
	"group":         str("Exclusive group: at most one active job per group."),
	"after":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Job ids that must succeed first."},
	"name":          str("Short label shown in status."),
	"key":           str("Idempotency key: if an unfinished job has this key, it is reused instead of starting another."),
	"keep":          map[string]any{"type": "boolean", "description": "Keep the job if this agent session ends (default: cancel it)."},
}

func tools() []tool {
	run := map[string]any{"wait": str("How long to wait for completion before returning, e.g. \"10m\" (default 10m, max 1h). If it is still running, call berth_wait.")}
	for k, v := range jobProps {
		run[k] = v
	}
	return []tool{
		{Name: "berth_run", Description: "Run a command as a resource-limited job on this host and wait for it. Returns exit code, the end of its output, and, if it had to wait, why. Use it for builds, tests, containers and anything heavy, instead of running them directly.",
			InputSchema: obj(run)},
		{Name: "berth_submit", Description: "Queue a command as a background job and return its id at once. Follow it with berth_wait or berth_logs.", InputSchema: obj(jobProps)},
		{Name: "berth_wait", Description: "Wait for a job to finish (or the wait to pass) and return its state, exit code and output tail.",
			InputSchema: obj(map[string]any{"id": str("Job id (or unique prefix)."), "wait": str("Maximum wait, default 10m, max 1h.")}, "id")},
		{Name: "berth_status", Description: "Show the queue: capacity, what fits right now, and every job with the reason it waits. With id, one job in detail.",
			InputSchema: obj(map[string]any{"id": str("Optional job id.")})},
		{Name: "berth_logs", Description: "Read a job's output.",
			InputSchema: obj(map[string]any{"id": str("Job id."), "tail": str("Bytes from the end, e.g. \"16K\" (default 16K)."), "offset": num("Read from this byte offset instead (paging: pass the returned next_offset).")}, "id")},
		{Name: "berth_cancel", Description: "Cancel a job (queued or running).",
			InputSchema: obj(map[string]any{"id": str("Job id."), "reason": str("Why, recorded on the job.")}, "id")},
	}
}

// Serve runs the protocol on in/out until in closes.
func Serve(in io.Reader, out io.Writer, version string) error {
	r := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var req rpcRequest
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			} else if resp := dispatch(req, version); resp != nil {
				_ = enc.Encode(resp)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func dispatch(req rpcRequest, version string) *rpcResponse {
	if len(req.ID) == 0 { // notification
		return nil
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "berth", "version": version},
			"instructions":    Instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		text, data, err := call(p.Name, p.Arguments)
		result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if data != nil {
			result["structuredContent"] = data
		}
		if err != nil {
			result["content"] = []map[string]any{{"type": "text", "text": "error: " + err.Error()}}
			result["isError"] = true
		}
		resp.Result = result
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// Instructions tell agents how to use berth; also printed by `berth agents`.
const Instructions = `berth runs heavy work (builds, tests, containers, large git operations) as
resource-limited jobs so many agents can share this machine without running it
out of memory or disk. Prefer berth_run over running heavy commands directly.

- Size requests realistically (memory "2G" for most builds; more only when needed).
  Smaller jobs start sooner: the queue backfills them past larger waiting jobs.
- If a job waits, the result says why and often how to start now
  ("would start now with --memory 3G or less").
- A job that can never fit while long-running services hold memory is reported as
  "unsatisfiable" at once: lower the request instead of waiting.
- Use "after" to order dependent jobs (e.g. init before add), "group" for jobs
  that must not overlap (same repository, same device), "key" to make retries
  idempotent, and "queue_timeout" when a late start is useless.
- Jobs are cancelled when this session ends unless "keep" is set.`

type args struct {
	Command      string   `json:"command"`
	Argv         []string `json:"argv"`
	Cwd          string   `json:"cwd"`
	Memory       string   `json:"memory"`
	CPUs         float64  `json:"cpus"`
	Timeout      string   `json:"timeout"`
	QueueTimeout string   `json:"queue_timeout"`
	GPUs         int      `json:"gpus"`
	Group        string   `json:"group"`
	After        []string `json:"after"`
	Name         string   `json:"name"`
	Key          string   `json:"key"`
	Keep         bool     `json:"keep"`
	Wait         string   `json:"wait"`
	ID           string   `json:"id"`
	Tail         string   `json:"tail"`
	Offset       *int64   `json:"offset"`
	Reason       string   `json:"reason"`
}

func (a *args) spec() (job.Spec, error) {
	s := job.Spec{Disk: -1, Name: a.Name, Key: a.Key, Group: a.Group, After: a.After, CPUs: a.CPUs, Env: os.Environ()}
	switch {
	case len(a.Argv) > 0:
		s.Argv = a.Argv
	case a.Command != "":
		s.Argv = []string{"/bin/sh", "-c", a.Command}
		if s.Name == "" {
			s.Name = a.Command
			if len(s.Name) > 60 {
				s.Name = s.Name[:59] + "…"
			}
		}
	default:
		return s, fmt.Errorf("give command or argv")
	}
	s.Cwd = a.Cwd
	if s.Cwd == "" {
		s.Cwd, _ = os.Getwd()
	}
	var err error
	if a.Memory != "" {
		if s.Memory, err = units.ParseBytes(a.Memory); err != nil {
			return s, err
		}
	}
	if s.Timeout, err = units.ParseDuration(a.Timeout); err != nil {
		return s, err
	}
	if s.QueueTimeout, err = units.ParseDuration(a.QueueTimeout); err != nil {
		return s, err
	}
	if a.GPUs > 0 {
		s.Resources = map[string]int{"gpu": a.GPUs}
	}
	return s, nil
}

func waitFor(a *args) time.Duration {
	d, err := units.ParseDuration(a.Wait)
	if err != nil || d <= 0 {
		d = 10 * time.Minute
	}
	return min(d, time.Hour)
}

func call(name string, raw json.RawMessage) (string, any, error) {
	var a args
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", nil, fmt.Errorf("invalid arguments: %v", err)
		}
	}
	switch name {
	case "berth_run", "berth_submit":
		spec, err := a.spec()
		if err != nil {
			return "", nil, err
		}
		resp, err := client.Submit(spec, !a.Keep, os.Getpid())
		if err != nil {
			return "", nil, err
		}
		if name == "berth_submit" {
			j := resp.Job
			return fmt.Sprintf("queued job %s (%s)%s", j.ID, j.Label(), reusedNote(resp)), j, nil
		}
		return finish(resp.Job.ID, waitFor(&a))
	case "berth_wait":
		return finish(a.ID, waitFor(&a))
	case "berth_status":
		resp, err := client.Call(api.Request{Op: "status", ID: a.ID})
		if err != nil {
			return "", nil, err
		}
		return statusText(resp.Status), resp.Status, nil
	case "berth_logs":
		if a.Offset != nil {
			resp, err := client.Call(api.Request{Op: "logs", ID: a.ID, Offset: *a.Offset})
			if err != nil {
				return "", nil, err
			}
			return resp.Data, map[string]any{"next_offset": resp.Offset}, nil
		}
		n := int64(16 << 10)
		if a.Tail != "" {
			if v, err := units.ParseBytes(a.Tail); err == nil {
				n = v
			}
		}
		out, err := client.Tail(a.ID, n)
		return out, nil, err
	case "berth_cancel":
		j, err := client.Cancel(a.ID, a.Reason)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("job %s: %s", job.Short(j.ID), j.State), j, nil
	}
	return "", nil, fmt.Errorf("unknown tool %q", name)
}

func reusedNote(r *api.Response) string {
	if r.Reused {
		return "; an unfinished job with this key already existed, reusing it"
	}
	return ""
}

func finish(id string, wait time.Duration) (string, any, error) {
	var waitedFor []string
	j, err := client.Watch(id, wait, func(r *api.Response) {
		if r.Event == "queued" && r.Job != nil && len(r.Job.Reasons) > 0 {
			waitedFor = append(waitedFor, client.Explain(r.Job))
		}
	})
	if j == nil {
		return "", nil, err
	}
	var b strings.Builder
	if !j.State.Terminal() {
		fmt.Fprintf(&b, "job %s is still %s after %s", j.ID, j.State, units.FormatDuration(wait))
		if j.State == job.Queued {
			fmt.Fprintf(&b, ": %s", client.Explain(j))
		}
		b.WriteString(". Call berth_wait again, or berth_cancel.\n")
		return b.String(), j, nil
	}
	dur := ""
	if !j.Started.IsZero() {
		dur = " in " + units.FormatDuration(j.Finished.Sub(j.Started))
	}
	fmt.Fprintf(&b, "job %s %s, exit code %d%s", job.Short(j.ID), j.State, client.ExitCode(j), dur)
	if j.Note != "" {
		fmt.Fprintf(&b, " (%s)", j.Note)
	}
	b.WriteString("\n")
	if len(waitedFor) > 0 {
		fmt.Fprintf(&b, "waited for: %s\n", waitedFor[len(waitedFor)-1])
	}
	if !j.Started.IsZero() {
		if tail, err := client.Tail(j.ID, 16<<10); err == nil && tail != "" {
			b.WriteString("--- output (last 16K) ---\n")
			b.WriteString(tail)
		}
	}
	return b.String(), j, nil
}

func statusText(s *api.Status) string {
	var b strings.Builder
	p := s.Pool
	fmt.Fprintf(&b, "pool: %s of %s memory charged, %g of %g CPUs; fits now: %s / %g CPUs; admission: %s\n",
		units.FormatBytes(p.MemoryCharged), units.FormatBytes(p.Memory), p.CPUsCharged, p.CPUs,
		units.FormatBytes(p.FitsNowMemory), p.FitsNowCPUs, s.Admission)
	for _, j := range s.Jobs {
		if j.State.Terminal() {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s/%gcpu %s", job.Short(j.ID), j.State, units.FormatBytes(j.Spec.Memory), j.Spec.CPUs, j.Label())
		if j.State == job.Queued {
			fmt.Fprintf(&b, ": %s", client.Explain(&j))
		}
		b.WriteString("\n")
	}
	return b.String()
}
