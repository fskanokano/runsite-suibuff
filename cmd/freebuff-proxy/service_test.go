package main

import (
	"strings"
	"testing"
)

// TestWindowsTaskCreateArgs pins the schtasks /create invocation: per-user
// on-logon task, non-elevated, the /tr wrapper cds into the executable's
// directory so ./.env resolves (matching start-proxy.cmd), /f overwrite.
func TestWindowsTaskCreateArgs(t *testing.T) {
	args := windowsTaskCreateArgs(`C:\tools\freebuff-proxy.exe`, `C:\tools`)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"/create",
		"/tn freebuff-proxy",
		"/sc onlogon",
		"/rl limited",
		"/f",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("windowsTaskCreateArgs missing %q in %q", want, joined)
		}
	}
	if !strings.Contains(joined, `cmd.exe /c cd /d "C:\tools" && "C:\tools\freebuff-proxy.exe"`) {
		t.Errorf("windowsTaskCreateArgs /tr wrapper wrong: %q", joined)
	}
}

// TestWindowsTaskStatusParsing pins the schtasks /v /fo LIST parser: a
// Running task registers+activates, a Ready task registers without
// activating, and output without the task registers nothing.
func TestWindowsTaskStatusParsing(t *testing.T) {
	cases := []struct {
		name           string
		out            string
		wantRegistered bool
		wantActive     bool
	}{
		{"running", "TaskName:   freebuff-proxy\nStatus:     Running\nTask To Run: cmd.exe ...\n", true, true},
		{"ready", "TaskName:   freebuff-proxy\nStatus:     Ready\n", true, false},
		{"disabled", "Status: Disabled", true, false},
		{"missing", "ERROR: The system cannot find the file specified.", false, false},
		{"empty", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registered, active := parseWindowsTaskStatus(tc.out)
			if registered != tc.wantRegistered || active != tc.wantActive {
				t.Errorf("parseWindowsTaskStatus(%q) = (%v,%v), want (%v,%v)",
					tc.out, registered, active, tc.wantRegistered, tc.wantActive)
			}
		})
	}
}

// TestSystemdUserUnit pins the unit content: WorkingDirectory and ExecStart
// point at the executable's directory and binary, and the unit is restartable
// and enable-able (Restart + WantedBy present).
func TestSystemdUserUnit(t *testing.T) {
	unit := systemdUserUnit("/opt/freebuff-proxy/freebuff-proxy", "/opt/freebuff-proxy")
	for _, want := range []string{
		"WorkingDirectory=/opt/freebuff-proxy",
		"ExecStart=/opt/freebuff-proxy/freebuff-proxy",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemdUserUnit missing %q in:\n%s", want, unit)
		}
	}
}

// TestSystemdActiveParsing pins the systemctl is-active parser: only the
// literal "active" state counts as running.
func TestSystemdActiveParsing(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"active", true},
		{"inactive", false},
		{"failed", false},
		{"activating", false},
		{"active\n", true},
	}
	for _, tc := range cases {
		if got := parseSystemdActive(tc.out); got != tc.want {
			t.Errorf("parseSystemdActive(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestLaunchdPlist pins the LaunchAgent content: label, ProgramArguments
// pointing at the binary, WorkingDirectory so ./.env resolves, and
// RunAtLoad+KeepAlive for autostart/respawn.
func TestLaunchdPlist(t *testing.T) {
	plist := launchdPlist("/usr/local/bin/freebuff-proxy", "/usr/local/bin")
	for _, want := range []string{
		"com.freebuff-proxy",
		"/usr/local/bin/freebuff-proxy",
		"WorkingDirectory",
		"/usr/local/bin",
		"RunAtLoad",
		"KeepAlive",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("launchdPlist missing %q in:\n%s", want, plist)
		}
	}
}

// TestLaunchctlListParsing pins the launchctl list parser: the row with the
// label and a numeric PID column means loaded; "-" PID or absent label does
// not.
func TestLaunchctlListParsing(t *testing.T) {
	const label = "com.freebuff-proxy"
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"loaded", "PID\tStatus\tLabel\n1234\t0\tcom.freebuff-proxy\n", true},
		{"loaded no header", "1234\t0\tcom.freebuff-proxy\n", true},
		{"unloaded dash", "-\t0\tcom.freebuff-proxy\n", false},
		{"absent", "PID\tStatus\tLabel\n1234\t0\tcom.other\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLaunchctlList(tc.out, label); got != tc.want {
				t.Errorf("parseLaunchctlList(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
