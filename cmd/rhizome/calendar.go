package main

// RHZ-137 (FR-RHZ-181): rhizome calendar export — fetches the board's
// GET /v1/calendar.ics and writes the bytes unchanged to -o or stdout.

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const calendarUsage = `usage:
  rhizome calendar export [-goal G] [-o path] [-addr A]

-o defaults to stdout. -addr defaults to RHIZOME_BOARD when set, else ` + scheduleDefaultAddr + ` (serve's default).
`

func calendarCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, calendarUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(out, calendarUsage)
		return 0
	case "export":
		return calendarExport(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "unknown calendar command %q\n\n%s", args[0], calendarUsage)
	return 2
}

func calendarExport(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("calendar export", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	goal := f.String("goal", "", "goal id")
	path := f.String("o", "", "output file (default stdout)")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprint(errOut, calendarUsage)
		return 2
	}
	u := scheduleBase(f, *addr) + "/v1/calendar.ics"
	if *goal != "" {
		u += "?goal=" + url.QueryEscape(*goal)
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
	if *path == "" {
		out.Write(body)
		return 0
	}
	if err := os.WriteFile(*path, body, 0o644); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}
