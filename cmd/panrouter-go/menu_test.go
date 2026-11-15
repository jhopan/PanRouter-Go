package main

import (
	"slices"

	"bufio"
	"github.com/urfave/cli/v2"
	"strings"
	"testing"
)

func TestParseChoice(t *testing.T) {
	cases := map[string]string{
		"":        "1", // Enter takes the default
		"\n":      "1",
		"1\n":     "1",
		"3":       "3",
		"6\n":     "6",
		" 5 \n":   "5",
		"0\n":     "0",
		"q\n":     "0",
		"bogus\n": "0",
	}
	for in, want := range cases {
		if got := parseChoice(in); got != want {
			t.Errorf("parseChoice(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReadChoiceEOFIsNotABlankLine: the caller tells "no operator at all" from
// "the operator pressed Ctrl+Z" by whether anything arrived, so an empty read
// has to be distinguishable from a blank line.
func TestReadChoiceEOFIsNotABlankLine(t *testing.T) {
	if line, ok := readChoice(bufio.NewReader(strings.NewReader(""))); ok {
		t.Errorf("readChoice on an empty stream = (%q, true), want ok=false", line)
	}
	if line, ok := readChoice(bufio.NewReader(strings.NewReader("\n"))); !ok || line != "\n" {
		t.Errorf("readChoice(blank line) = (%q, %v), want (\\n, true)", line, ok)
	}
	// A line with no trailing newline still carries the operator's input.
	if line, ok := readChoice(bufio.NewReader(strings.NewReader("3"))); !ok || line != "3" {
		t.Errorf("readChoice(\"3\" at EOF) = (%q, %v), want (3, true)", line, ok)
	}
}

func TestTrayBinaryName(t *testing.T) {
	name := trayBinaryName()
	if !strings.HasSuffix(name, "-tray") {
		t.Errorf("expected tray binary name to end with -tray, got %q", name)
	}
}

// TestServiceArgsAlwaysAskForTheServerPath pins the argument the autostart entry
// carries.
//
// Without --serve the supervised instance fell into the interactive picker,
// because Windows gives a scheduled task a console and the CLI treated a console
// as "a human is here". The task then reported Active: true while the process sat
// on the menu: no port bound, no boot line in the run log, and `service status`
// said everything was fine.
func TestServiceArgsAlwaysAskForTheServerPath(t *testing.T) {
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "rtk"},
			&cli.BoolFlag{Name: "serve"},
		},
		Action: func(cCtx *cli.Context) error {
			args := serviceArgs(cCtx)
			if slices.Index(args, "--serve") == -1 {
				t.Errorf("serviceArgs() = %v, want --serve in the list", args)
			}
			// A second --serve would be harmless but sloppy: the flag must not
			// be appended when it is already carried.
			if n := len(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a != "--serve" })); n != 1 {
				t.Errorf("serviceArgs() = %v, want exactly one --serve", args)
			}
			return nil
		},
	}
	if err := app.Run([]string{"panrouter-go", "--rtk=true"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := app.Run([]string{"panrouter-go", "--serve", "--rtk=true"}); err != nil {
		t.Fatalf("run with an explicit --serve: %v", err)
	}
}
