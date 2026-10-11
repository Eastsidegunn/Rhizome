package main

// RHZ-145 (FR-RHZ-189): rhizome procedure define|run|status — ops defines
// steps, order and gates; the board's procedure executor runs them. Like
// schedule, it only talks to a running board over HTTP.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const procedureUsage = `usage:
  rhizome procedure define -f steps.json [-addr A]
  rhizome procedure run <procedureId> -goal G [-name RUN] [-param k=v ...] [-addr A]
  rhizome procedure status <runId> [-addr A]

define: steps.json is the procedure.define body (id, sourceKnowledgeId,
trigger, steps[{id, action, after, needsGate, recommendation, instruction,
workdir, sessionMode, model}], ...).
run: creates the run with execute=true — the board's executor starts each
step through the session launcher once all its "after" steps succeeded (and
its gate is approved), and stops the run (blocked + attention) when a step
fails or is cancelled. Step workdir/sessionMode/model fall back to the run
params of the same name. -name defaults to <procedureId>-<UTC yyyymmddHHMMSS>.
-addr defaults to RHIZOME_BOARD when set, else ` + scheduleDefaultAddr + `.
`

func procedureCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(out, procedureUsage)
		return 0
	case "define":
		return procedureDefine(args[1:], out, errOut)
	case "run":
		return procedureRun(args[1:], out, errOut)
	case "status":
		return procedureStatus(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "unknown procedure command %q\n\n%s", args[0], procedureUsage)
	return 2
}

type paramFlags map[string]string

func (p paramFlags) String() string { return "" }
func (p paramFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("param must be k=v")
	}
	p[k] = val
	return nil
}

// procedurePost sends one intent and reports a rejection.
func procedurePost(base string, body map[string]any, errOut io.Writer) int {
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/v1/intent", "application/json", bytes.NewReader(b))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer resp.Body.Close()
	var res struct {
		Accepted bool
		Reason   string
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil {
		fmt.Fprintf(errOut, "board answered %s\n", resp.Status)
		return 1
	}
	if !res.Accepted {
		fmt.Fprintf(errOut, "rejected: %s\n", res.Reason)
		return 1
	}
	return 0
}

func procedureDefine(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("procedure define", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	file := f.String("f", "", "procedure.define JSON body")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if *file == "" || f.NArg() != 0 {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		fmt.Fprintf(errOut, "%s: not a JSON object\n", *file)
		return 1
	}
	body["kind"], body["actor"] = "procedure.define", "cli"
	if rc := procedurePost(scheduleBase(f, *addr), body, errOut); rc != 0 {
		return rc
	}
	fmt.Fprintln(out, body["id"])
	return 0
}

func procedureRun(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	id := args[0]
	f := flag.NewFlagSet("procedure run", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	goal := f.String("goal", "", "goal id")
	name := f.String("name", "", "run id")
	params := paramFlags{}
	f.Var(params, "param", "run param k=v (repeatable)")
	if err := f.Parse(args[1:]); err != nil {
		return 2
	}
	if *goal == "" || f.NArg() != 0 {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	run := *name
	if run == "" {
		run = id + "-" + time.Now().UTC().Format("20060102150405")
	}
	body := map[string]any{"kind": "procedure.run", "actor": "cli", "id": id, "name": run, "goalId": *goal, "execute": true}
	if len(params) > 0 {
		body["params"] = map[string]string(params)
	}
	if rc := procedurePost(scheduleBase(f, *addr), body, errOut); rc != 0 {
		return rc
	}
	fmt.Fprintln(out, run)
	return 0
}

func procedureStatus(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	run := args[0]
	f := flag.NewFlagSet("procedure status", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	if err := f.Parse(args[1:]); err != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprint(errOut, procedureUsage)
		return 2
	}
	resp, err := http.Get(scheduleBase(f, *addr) + "/v1/procedure/runs/" + url.PathEscape(run))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(errOut, "board answered %s: %s", resp.Status, msg)
		return 1
	}
	var rs struct {
		RunID, MissionID, ProcedureID, State, Stalled string
		Driven                                        bool
		Done, Total                                   int
		Steps                                         []struct {
			ID, MissionState, Phase, Gate string
			After                         []string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(out, "run %s (%s) procedure %s state %s steps %d/%d", rs.RunID, rs.MissionID, rs.ProcedureID, rs.State, rs.Done, rs.Total)
	if !rs.Driven {
		fmt.Fprint(out, " [not driven]")
	}
	fmt.Fprintln(out)
	if rs.Stalled != "" {
		fmt.Fprintf(out, "stalled: %s\n", rs.Stalled)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tSTATE\tPHASE\tGATE\tAFTER")
	for _, s := range rs.Steps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.MissionState, dash(s.Phase), dash(s.Gate), dash(strings.Join(s.After, ",")))
	}
	tw.Flush()
	return 0
}
