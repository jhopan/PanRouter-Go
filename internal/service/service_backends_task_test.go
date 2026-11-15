package service

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWindowsTaskGuardsOffWindows(t *testing.T) {
	stubHost(t, hostStub{goos: "linux", euid: 1000})
	stubRun(t)

	if _, err := installWindowsTask("C:\\panrouter-go.exe", nil); err == nil {
		t.Error("installWindowsTask() error = nil off windows, want the guard error")
	}
	if _, ok := uninstallWindowsTask(); ok {
		t.Error("uninstallWindowsTask() ok = true off windows, want false")
	}
	if _, ok := statusWindowsTask(); ok {
		t.Error("statusWindowsTask() ok = true off windows, want false")
	}
}

func TestInstallWindowsTaskOnWindows(t *testing.T) {
	stubHost(t, hostStub{goos: "windows"})
	stubWindowsTaskUser(t, "bima")
	t.Setenv("USERNAME", "bima")

	// The definition is written to a temp file and handed to schtasks, so the
	// stub reads it back through the argument it is given.
	var definition string
	calls := stubRunFunc(t, func(name string, args ...string) (string, error) {
		if strings.Contains(name, "powershell") {
			// The task answers "Running" (ordinal 4) after /run.
			return "4", nil
		}
		if i := slices.Index(args, "/xml"); i >= 0 && i+1 < len(args) {
			if data, err := os.ReadFile(args[i+1]); err == nil {
				// The file is UTF-16LE with a BOM, which is what schtasks
				// accepts; decode it to compare against the rendered form.
				units := make([]uint16, 0, len(data)/2)
				for j := 2; j+1 < len(data); j += 2 {
					units = append(units, uint16(data[j])|uint16(data[j+1])<<8)
				}
				definition = string(utf16.Decode(units))
			}
		}
		return "", nil
	})

	info, err := installWindowsTask(`C:\panrouter-go.exe`, []string{"--rtk=true"})
	if err != nil {
		t.Fatalf("installWindowsTask() error = %v", err)
	}
	if info.Backend != BackendWindowsTask || !info.Enabled || !info.Active {
		t.Errorf("Info = %+v, want windows-task enabled and active", info)
	}
	if info.Path != windowsTaskName {
		t.Errorf("Info.Path = %q, want %q", info.Path, windowsTaskName)
	}

	var create *svcCall
	for i := range *calls {
		if (*calls)[i].name == "schtasks.exe" && slices.Contains((*calls)[i].args, "/create") {
			create = &(*calls)[i]
			break
		}
	}
	if create == nil {
		t.Fatalf("calls = %v, want a schtasks /create", *calls)
	}
	// The task body comes from the definition file, not from the command line:
	// /tr cannot express the battery conditions, the execution limit or the
	// working directory, and their defaults are what stopped the autostart.
	if !slices.Contains(create.args, "/xml") || !slices.Contains(create.args, "/f") {
		t.Errorf("schtasks /create args = %v, want /xml <file> /f", create.args)
	}
	for _, flag := range []string{"/tr", "/sc", "/it", "/ru", "/rp", "/rl"} {
		if slices.Contains(create.args, flag) {
			t.Errorf("schtasks /create args = %v, must not use %s", create.args, flag)
		}
	}
	if definition == "" {
		t.Fatal("the task definition was not readable through the /xml argument")
	}
	for _, want := range []string{
		"<UserId>bima</UserId>", // never SYSTEM: that reads a different %APPDATA%
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<Command>C:\\panrouter-go.exe</Command>",
		"<Arguments>--rtk=true</Arguments>",
		"<WorkingDirectory>" + windowsTaskWorkDir() + "</WorkingDirectory>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
	} {
		if !strings.Contains(definition, want) {
			t.Errorf("task definition is missing %s:\n%s", want, definition)
		}
	}
	if _, err := os.Stat(argAfter(create.args, "/xml")); err == nil {
		t.Error("the task definition file was left behind")
	}
}

// TestWindowsTaskXMLKeepsTheGatewayAliveOnBattery pins the three settings whose
// defaults made the autostart silently useless.
//
// Observed on a laptop running on its battery: `schtasks /create` succeeds,
// `schtasks /run` succeeds, the task sits in "Queued" with Last Result 0, and no
// process ever appears — `service start` reported exactly that ("started via
// windows-task (active: false)"). DisallowStartIfOnBatteries defaults to true.
// StopIfGoingOnBatteries would then kill the gateway on an unplug, and
// ExecutionTimeLimit (PT72H) would end it after three days.
func TestWindowsTaskXMLKeepsTheGatewayAliveOnBattery(t *testing.T) {
	xml := windowsTaskXML(`C:\bin\panrouter-go.exe`, nil, "bima", `C:\checkout`)
	for _, want := range []string{
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<StartWhenAvailable>true</StartWhenAvailable>",
		"<WorkingDirectory>C:\\checkout</WorkingDirectory>",
		"<LogonTrigger>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("definition missing %s", want)
		}
	}
	if strings.Contains(xml, "<Arguments>") {
		t.Error("an empty argument list must not emit an empty Arguments element")
	}

	// A path with XML-significant characters must survive as text, not break the
	// definition: schtasks rejects a malformed file and install then fails.
	escaped := windowsTaskXML(`C:\a&b\panrouter-go.exe`, []string{"--name=<x>"}, "", "")
	if !strings.Contains(escaped, "C:\\a&amp;b\\panrouter-go.exe") || !strings.Contains(escaped, "--name=&lt;x&gt;") {
		t.Errorf("definition did not escape XML-significant characters: %s", escaped)
	}
}

// TestWindowsCommandLineQuotesForCmd: the /tr value is read by cmd.exe, so a
// spaced path is wrapped verbatim, not escaped the way strconv.Quote would.
func TestWindowsCommandLineQuotesForCmd(t *testing.T) {
	got := windowsCommandLine(`C:\Program Files\panrouter\panrouter-go.exe`, []string{"--rtk=true"})
	want := `"C:\Program Files\panrouter\panrouter-go.exe" --rtk=true`
	if got != want {
		t.Errorf("windowsCommandLine() = %q, want %q", got, want)
	}
	if strings.Contains(got, `\\`) {
		t.Errorf("windowsCommandLine() = %q, must not escape backslashes for cmd.exe", got)
	}
}

func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestInstallWindowsTaskErrorPaths(t *testing.T) {
	t.Run("create failure is reported and not enabled", func(t *testing.T) {
		stubHost(t, hostStub{goos: "windows"})
		stubRunFunc(t, func(name string, args ...string) (string, error) {
			if slices.Contains(args, "/create") {
				return "access denied", errors.New("boom")
			}
			return "", nil
		})

		info, err := installWindowsTask(`C:\panrouter-go.exe`, nil)
		if err == nil {
			t.Fatal("installWindowsTask() error = nil when /create fails")
		}
		if info.Enabled {
			t.Errorf("Info.Enabled = true after a failed create: %+v", info)
		}
	})

	t.Run("run failure leaves the task enabled but inactive", func(t *testing.T) {
		stubHost(t, hostStub{goos: "windows"})
		stubRunFunc(t, func(name string, args ...string) (string, error) {
			if slices.Contains(args, "/run") {
				return "", errors.New("boom")
			}
			return "", nil
		})

		info, err := installWindowsTask(`C:\panrouter-go.exe`, nil)
		if err != nil {
			t.Fatalf("installWindowsTask() error = %v", err)
		}
		if !info.Enabled || info.Active {
			t.Errorf("Info = %+v, want enabled but inactive", info)
		}
	})
}

func TestStatusWindowsTask(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		probeErr error
		wantOK   bool
		wantAct  bool
		wantUnk  bool
	}{
		{name: "task present and running", state: "4", wantOK: true, wantAct: true},
		// Ready is ordinal 3; matching the localized "Running" string broke
		// non-English Windows.
		{name: "task present but ready", state: "3", wantOK: true, wantAct: false},
		{name: "task disabled", state: "1", wantOK: true, wantAct: false},
		{name: "query error", probeErr: errors.New("no task"), wantOK: false},
		// PowerShell absent or blocked: report the state unknown, not false.
		{name: "unprobeable state is reported as unknown", state: "", wantOK: true, wantUnk: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubHost(t, hostStub{goos: "windows"})
			stubRunFunc(t, func(name string, args ...string) (string, error) {
				if tt.probeErr != nil {
					return "", tt.probeErr
				}
				if strings.Contains(name, "powershell") {
					return tt.state, nil
				}
				return windowsTaskName + "  Ready", nil
			})

			info, ok := statusWindowsTask()
			if ok != tt.wantOK {
				t.Fatalf("statusWindowsTask() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if info.Active != tt.wantAct || info.ActiveUnknown != tt.wantUnk {
				t.Errorf("Active = %v, ActiveUnknown = %v, want %v, %v", info.Active, info.ActiveUnknown, tt.wantAct, tt.wantUnk)
			}
		})
	}
}

func TestUninstallWindowsTask(t *testing.T) {
	t.Run("task present is ended and deleted", func(t *testing.T) {
		stubHost(t, hostStub{goos: "windows"})
		calls := stubRunFunc(t, func(name string, args ...string) (string, error) {
			if slices.Contains(args, "/query") {
				return windowsTaskName + "  Ready", nil
			}
			return "", nil
		})

		if _, ok := uninstallWindowsTask(); !ok {
			t.Fatal("uninstallWindowsTask() = false, want true")
		}
		var ended, deleted bool
		for _, c := range *calls {
			if slices.Contains(c.args, "/end") {
				ended = true
			}
			if slices.Contains(c.args, "/delete") {
				deleted = true
			}
		}
		if !ended || !deleted {
			t.Errorf("calls = %v, want /end then /delete", *calls)
		}
	})

	t.Run("query error means nothing to remove", func(t *testing.T) {
		stubHost(t, hostStub{goos: "windows"})
		stubRunFunc(t, func(string, ...string) (string, error) { return "", errors.New("not found") })
		if _, ok := uninstallWindowsTask(); ok {
			t.Error("uninstallWindowsTask() = true when the task does not exist")
		}
	})

	t.Run("output without the task name means nothing to remove", func(t *testing.T) {
		stubHost(t, hostStub{goos: "windows"})
		stubRunFunc(t, func(string, ...string) (string, error) { return "some other task", nil })
		if _, ok := uninstallWindowsTask(); ok {
			t.Error("uninstallWindowsTask() = true without the task name in the query output")
		}
	})
}

func TestControlWindowsTask(t *testing.T) {
	tests := []struct {
		op    string
		flags []string
	}{
		{"start", []string{"/run"}},
		{"stop", []string{"/end"}},
		{"restart", []string{"/end", "/run"}},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			calls := stubRun(t)
			if err := controlWindowsTask(tt.op); err != nil {
				t.Fatalf("controlWindowsTask(%s) error = %v", tt.op, err)
			}
			for _, flag := range tt.flags {
				found := false
				for _, c := range *calls {
					if slices.Contains(c.args, flag) {
						found = true
					}
				}
				if !found {
					t.Errorf("calls = %v, want a %s invocation", *calls, flag)
				}
			}
		})
	}

	t.Run("unknown operation is rejected", func(t *testing.T) {
		stubRun(t)
		if err := controlWindowsTask("explode"); err == nil {
			t.Error("controlWindowsTask(explode) error = nil, want error")
		}
	})
}

// TestWindowsTaskXMLBytesAreUTF16 pins the encoding schtasks demands.
//
// The first version wrote UTF-8 and schtasks refused it outright:
// `ERROR: The task XML is malformed. (1,40)::ERROR: unable to switch the
// encoding`. The parser accepts only a UTF-16 file whose declaration says so, so
// the bytes carry a BOM and the string form declares UTF-16.
func TestWindowsTaskXMLBytesAreUTF16(t *testing.T) {
	definition := windowsTaskXMLBytes(`C:\\bin\\panrouter-go.exe`, []string{"--log=info"}, "bima", `C:\\checkout`)

	if len(definition) < 2 || definition[0] != 0xFF || definition[1] != 0xFE {
		t.Fatalf("definition does not start with a UTF-16LE BOM: % x", definition[:min(len(definition), 8)])
	}
	if len(definition)%2 != 0 {
		t.Fatalf("UTF-16 output has an odd length: %d", len(definition))
	}

	units := make([]uint16, 0, len(definition)/2)
	for i := 2; i+1 < len(definition); i += 2 {
		units = append(units, uint16(definition[i])|uint16(definition[i+1])<<8)
	}
	decoded := string(utf16.Decode(units))

	if decoded != windowsTaskXML(`C:\\bin\\panrouter-go.exe`, []string{"--log=info"}, "bima", `C:\\checkout`) {
		t.Errorf("decoded definition differs from the rendered one:\n%s", decoded)
	}
	if !strings.Contains(decoded, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("the declaration must name the encoding the bytes use: %s", decoded[:60])
	}
}
