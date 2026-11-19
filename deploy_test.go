package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The cycle deadline the binary falls back to, in the seconds a unit file has to
// exceed before systemd would kill a shutdown mid-write.
var defaultFetchTimeoutSeconds = int(defaultFetchTimeout / time.Second)

// unitLines returns the settings of a unit file with comments stripped, because a
// documentation line that mentions a flag is not the service using it.
func unitLines(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no unit file: %v", err)
	}
	// systemd allows a setting to continue over lines with a trailing backslash.
	raw := strings.ReplaceAll(string(data), "\\\n", " ")
	settings := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("not a setting: %q", line)
			continue
		}
		settings[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return settings
}

// nohup keeps the process alive across a logout, but not across a reboot, and a
// systemd-logind session with KillUserProcesses=yes terminates what is left when
// the session ends - which nohup cannot ignore, because it arrives as SIGTERM. So
// there is a unit file, and these checks keep it honest about the conventions the
// binary relies on.
func TestSystemdUnitMatchesHowTheBinaryWantsToRun(t *testing.T) {
	settings := unitLines(t, filepath.Join("deploy", "rssfetcher.service"))
	unit := strings.Join(linesOf(settings), "\n")

	if settings["Type"] != "simple" {
		t.Errorf("Type=%q, want simple: a forking service loses stdout and the restart policy", settings["Type"])
	}
	// The background mode detaches the server from its supervisor, which would make
	// systemd think the service exited cleanly and leave the real process running.
	for _, key := range []string{"ExecStart", "Environment", "EnvironmentFile"} {
		if strings.Contains(settings[key], "RSSFETCHER_BACKGROUND") {
			t.Errorf("%s uses the background mode; systemd is the supervisor here", key)
		}
	}
	if settings["EnvironmentFile"] == "" {
		t.Error("the unit does not load its configuration from a file the operator can chmod 600")
	}
	if !strings.Contains(settings["Restart"], "always") && !strings.Contains(settings["Restart"], "on-failure") {
		t.Error("the unit does not restart the service")
	}
	for _, forbidden := range []string{"-admin-password", "-auth-secret", "-address ", "MONGO_USER"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("the unit puts credentials in the command line (%s); ps(1) is world readable", forbidden)
		}
	}

	// Shutdown lets an in-flight fetch cycle finish its bulk write before
	// disconnecting the mongo client, which takes up to the cycle deadline.
	stop := settings["TimeoutStopSec"]
	if stop == "" {
		t.Fatal("the unit does not set TimeoutStopSec")
	}
	seconds, err := strconv.Atoi(strings.TrimSuffix(stop, "s"))
	if err != nil {
		t.Fatalf("TimeoutStopSec=%q is not a number of seconds", stop)
	}
	if seconds < defaultFetchTimeoutSeconds {
		t.Errorf("TimeoutStopSec=%s is shorter than the %ds a cycle may take; shutdown would be killed mid-write",
			stop, defaultFetchTimeoutSeconds)
	}
}

// The example env file is the thing people copy, so it must be obviously empty.
func TestExampleEnvironmentHoldsNoSecrets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("deploy", "env.example"))
	if err != nil {
		t.Fatalf("no example environment: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("not a KEY=value line: %q", line)
			continue
		}
		secret := strings.Contains(name, "PASSWORD") || strings.Contains(name, "SECRET") || strings.Contains(name, "USER") || strings.Contains(name, "URI")
		if secret && value != "" {
			t.Errorf("deploy/env.example gives %s a value; the example must stay empty", name)
		}
	}
	// And every variable it names has to be one the program actually reads, or the
	// example teaches people to set something that does nothing.
	declared := map[string]bool{}
	for _, name := range []string{
		"MONGO_URI", "RSSFETCHER_ADMIN_USER", "RSSFETCHER_ADMIN_PASSWORD", "RSSFETCHER_AUTH_SECRET",
		"RSSFETCHER_LISTEN", "RSSFETCHER_INTERVAL", "RSSFETCHER_JITTER", "RSSFETCHER_MAX_FEED_TTL",
		"RSSFETCHER_FETCH_TIMEOUT", "RSSFETCHER_CHECK_TIMEOUT", "RSSFETCHER_MAX_ITEMS",
		"RSSFETCHER_COOKIE_SECURE", "RSSFETCHER_IGNORE_FEED_TTL",
	} {
		declared[name] = true
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, "=")
		if !declared[name] {
			t.Errorf("deploy/env.example names %s, which nothing reads", name)
		}
	}
}

// The two documents describe the same two ways of starting the server, and the
// README is where an operator actually looks.
func TestStartScriptsAndUnitDocumentEachOther(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	for _, want := range []string{
		"RSSFETCHER_BACKGROUND",
		"deploy/rssfetcher.service",
		"deploy/env.example",
		"loginctl enable-linger",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("README does not mention %s", want)
		}
	}
}

func linesOf(settings map[string]string) []string {
	out := make([]string, 0, len(settings))
	for _, value := range settings {
		out = append(out, value)
	}
	return out
}
