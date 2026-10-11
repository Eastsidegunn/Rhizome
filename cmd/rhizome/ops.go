package main

// RHZ-146 (FR-RHZ-190): rhizome ops brief — fetches the board's
// GET /v1/ops/brief and writes the bytes unchanged to stdout.

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const opsUsage = `usage:
  rhizome ops brief [-json] [-addr A]

Prints the board's deterministic boot summary (markdown; -json for JSON).
-addr defaults to RHIZOME_BOARD when set, else ` + scheduleDefaultAddr + ` (serve's default).
`

func opsCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, opsUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(out, opsUsage)
		return 0
	case "brief":
		return opsBrief(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "unknown ops command %q\n\n%s", args[0], opsUsage)
	return 2
}

func opsBrief(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("ops brief", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	asJSON := f.Bool("json", false, "JSON instead of markdown")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprint(errOut, opsUsage)
		return 2
	}
	u := scheduleBase(f, *addr) + "/v1/ops/brief"
	if *asJSON {
		u += "?format=json"
	}
	resp, err := http.Get(u)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "board answered %s: %s\n", resp.Status, strings.TrimSpace(string(body)))
		return 1
	}
	out.Write(body)
	return 0
}
