package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

// windowsTaskName is the scheduled-task name this package owns.
const windowsTaskName = "panrouter-go"

// windowsTaskUser resolves the account the logon task runs as. It is the
// interactive user that ran install, never SYSTEM: a SYSTEM task reads
// %APPDATA%\panrouter — a different data directory from the user's, silently.
// It is a seam for tests.
var windowsTaskUser = func() string {
	for _, key := range []string{"USERNAME", "USER"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// windowsQuoteArg quotes one argv element for the cmd.exe command line
// schtasks stores in its /tr value. strconv.Quote (used for the POSIX targets)
// escapes backslashes, which cmd.exe does not understand: it would see
// "C:\\Program Files\\panrouter-go.exe" as a path it cannot run. An element
// without spaces or tabs is safe verbatim.
func windowsQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t") {
		return arg
	}
	return `"` + arg + `"`
}

// windowsCommandLine joins the executable and args into the single command
// line schtasks /tr expects, using cmd.exe quoting rather than POSIX shell
// quoting. It is deliberately separate from commandLine, which stays correct
// for the sh, cron, OpenRC, runit, and systemd targets.
func windowsCommandLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, windowsQuoteArg(exe))
	for _, arg := range args {
		parts = append(parts, windowsQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

// windowsTaskWorkDir is the directory the task starts the gateway in. It is the
// directory install ran from — the checkout — so the `.env` an operator keeps
// beside the source is the one the boot-time instance reads. Without it the task
// inherits %SystemRoot%\System32 and reads no .env at all; the port silently
// became the default and the operator's settings were ignored.
var windowsTaskWorkDir = func() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// windowsTaskXML renders the task definition schtasks /create /xml registers.
//
// It exists instead of the /tr form for three settings the command line cannot
// express, each of which silently broke the autostart on a laptop:
//
//   - DisallowStartIfOnBatteries defaults to true. On a machine running on its
//     battery `schtasks /run` accepts the request, the task sits in "Queued",
//     the last result stays 0 — and nothing starts. That is what `service start`
//     looked like from the outside: "started via windows-task (active: false)".
//   - StopIfGoingOnBatteries defaults to true, so an unplug would kill the
//     gateway mid-request even if it did start.
//   - ExecutionTimeLimit defaults to PT72H: a gateway is meant to outlive three
//     days, and the default ends it.
//
// WorkingDirectory is the third: see windowsTaskWorkDir. StartWhenAvailable and
// the one-minute restart make the entry behave like the supervision the POSIX
// backends get from their service manager.
func windowsTaskXML(exe string, args []string, user, workDir string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	b.WriteString(`<RegistrationInfo><Description>PanRouter-Go gateway</Description></RegistrationInfo>`)
	b.WriteString(`<Triggers><LogonTrigger><Enabled>true</Enabled>`)
	if user != "" {
		b.WriteString(`<UserId>` + xmlEscape(user) + `</UserId>`)
	}
	b.WriteString(`</LogonTrigger></Triggers>`)
	b.WriteString(`<Principals><Principal id="Author">`)
	if user != "" {
		b.WriteString(`<UserId>` + xmlEscape(user) + `</UserId>`)
	}
	b.WriteString(`<LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>`)
	b.WriteString(`<Settings>`)
	b.WriteString(`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`)
	b.WriteString(`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`)
	b.WriteString(`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`)
	b.WriteString(`<AllowHardTerminate>true</AllowHardTerminate>`)
	b.WriteString(`<StartWhenAvailable>true</StartWhenAvailable>`)
	b.WriteString(`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`)
	b.WriteString(`<Enabled>true</Enabled>`)
	b.WriteString(`<RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>`)
	b.WriteString(`</Settings>`)
	b.WriteString(`<Actions Context="Author"><Exec>`)
	b.WriteString(`<Command>` + xmlEscape(exe) + `</Command>`)
	if len(args) > 0 {
		b.WriteString(`<Arguments>` + xmlEscape(strings.Join(args, " ")) + `</Arguments>`)
	}
	if workDir != "" {
		b.WriteString(`<WorkingDirectory>` + xmlEscape(workDir) + `</WorkingDirectory>`)
	}
	b.WriteString(`</Exec></Actions>`)
	b.WriteString(`</Task>`)
	return b.String()
}

// windowsTaskXMLBytes renders the definition the way schtasks reads it.
//
// The file has to be UTF-16LE with a BOM: schtasks' XML parser refuses a
// declaration that is not UTF-16 and answers `unable to switch the encoding`
// for a UTF-8 file, which is what the first version of this sent.
func windowsTaskXMLBytes(exe string, args []string, user, workDir string) []byte {
	return utf16LEWithBOM(windowsTaskXML(exe, args, user, workDir))
}

// utf16LEWithBOM encodes a string as UTF-16 little endian with a byte-order
// mark, surrogate pairs included. encoding/xml has no encoder for a non-UTF-8
// target without pulling in x/text, and this is the whole requirement.
func utf16LEWithBOM(value string) []byte {
	out := make([]byte, 0, 2+2*len(value))
	out = append(out, 0xFF, 0xFE)
	for _, unit := range utf16.Encode([]rune(value)) {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}

// xmlEscape escapes one XML text node or attribute value.
func xmlEscape(value string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return value
	}
	return buf.String()
}

func installWindowsTask(exe string, args []string) (Info, error) {
	if goos != "windows" {
		return Info{}, fmt.Errorf("service.Install: windows-task backend only runs on windows")
	}
	taskCmd := windowsCommandLine(exe, args)
	user := windowsTaskUser()
	workDir := windowsTaskWorkDir()

	definition := windowsTaskXMLBytes(exe, args, user, workDir)
	file, err := os.CreateTemp("", "panrouter-go-task-*.xml")
	if err != nil {
		return Info{Backend: BackendWindowsTask, Exec: taskCmd}, fmt.Errorf("service.Install: write task definition: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(definition); err != nil {
		_ = file.Close()
		return Info{Backend: BackendWindowsTask, Exec: taskCmd}, fmt.Errorf("service.Install: write task definition: %w", err)
	}
	if err := file.Close(); err != nil {
		return Info{Backend: BackendWindowsTask, Exec: taskCmd}, fmt.Errorf("service.Install: write task definition: %w", err)
	}

	// schtasks /create /xml carries the settings /tr cannot express. /f replaces
	// an existing registration so a re-install is not a failure.
	if _, err := run("schtasks.exe", "/create", "/tn", windowsTaskName, "/xml", file.Name(), "/f"); err != nil {
		return Info{Backend: BackendWindowsTask, Exec: taskCmd}, err
	}

	info := Info{Backend: BackendWindowsTask, Path: windowsTaskName, Exec: taskCmd, Enabled: true}
	if _, err := run("schtasks.exe", "/run", "/tn", windowsTaskName); err != nil {
		return info, nil
	}
	// The run request being accepted is not the gateway starting: a task blocked
	// by a condition (battery, session) is queued and reports success. Ask the
	// task itself, which is the same probe status uses.
	active, known := windowsTaskRunning(windowsTaskName)
	info.Active = active
	info.ActiveUnknown = !known
	return info, nil
}

func uninstallWindowsTask() (Info, bool) {
	if goos != "windows" {
		return Info{}, false
	}
	out, err := run("schtasks.exe", "/query", "/tn", windowsTaskName)
	if err != nil || !strings.Contains(out, windowsTaskName) {
		return Info{}, false
	}
	_, _ = run("schtasks.exe", "/end", "/tn", windowsTaskName)
	_, _ = run("schtasks.exe", "/delete", "/tn", windowsTaskName, "/f")
	return Info{Backend: BackendWindowsTask, Path: windowsTaskName}, true
}

func statusWindowsTask() (Info, bool) {
	if goos != "windows" {
		return Info{}, false
	}
	out, err := run("schtasks.exe", "/query", "/tn", windowsTaskName)
	if err != nil || !strings.Contains(out, windowsTaskName) {
		return Info{}, false
	}
	active, known := windowsTaskRunning(windowsTaskName)
	return Info{
		Backend:       BackendWindowsTask,
		Path:          windowsTaskName,
		Enabled:       true,
		Active:        active,
		ActiveUnknown: !known,
	}, true
}

// windowsTaskRunning states whether the task is executing right now. It asks
// PowerShell for the TaskState as a number instead of matching "Running" in
// schtasks output, which is localised: on a non-English Windows that literal
// never appears, so `service status` reported Active: false for a task that
// was running. Casting the enum to int is itself locale-independent — the
// enum's ordinal values (Running=4) are fixed by the CIM type, not by the UI
// language, and PS 5.1 ships on every supported Windows.
//
// known is false when the probe itself could not answer — PowerShell missing,
// or blocked by execution policy. The caller then reports an unknown active
// state rather than the false that an absent probe used to imply.
func windowsTaskRunning(name string) (active, known bool) {
	quoted := strings.ReplaceAll(name, "'", "''")
	query := "(Get-ScheduledTask -TaskName '" + quoted + "' -ErrorAction SilentlyContinue).State"
	script := "$s = " + query + "; if ($null -eq $s) { '' } else { [int]$s }"
	out, err := run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return false, false
	}
	state := strings.TrimSpace(out)
	if state == "" {
		// The task vanished between the schtasks query and this probe.
		return false, false
	}
	// TaskState.Running is ordinal 4 in the CIM ScheduledTask type.
	return state == "4", true
}
