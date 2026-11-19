package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A server that always runs in the foreground of the shell that started it is
// wrong for a bare "ssh in and start it" deployment, and a server that always
// detaches is wrong for systemd and containers (a forking daemon loses stdout,
// and a supervisor cannot tell when it dies). So start-prod.sh keeps the
// foreground as the default and offers an explicit background mode, and this
// test pins both halves.
func TestStartProdScriptRunsBothWays(t *testing.T) {
	script, err := os.ReadFile("start-prod.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)

	for _, want := range []string{
		"exec ./rssfetcher",     // foreground by default
		"RSSFETCHER_BACKGROUND", // opt-in
		"nohup",                 // detached
		"run/rssfetcher.pid",    // so it can be stopped again
		"refusing to start: rssfetcher is already running", // no second instance
	} {
		if !strings.Contains(text, want) {
			t.Errorf("start-prod.sh does not contain %q", want)
		}
	}
	if !strings.Contains(text, "kill -0") {
		t.Error("start-prod.sh does not check whether the pid in the pidfile is alive")
	}
	// The detached process must not die when the terminal does, and its output
	// must not be thrown away.
	if !strings.Contains(text, "rssfetcher.log") {
		t.Error("background mode does not redirect the log somewhere findable")
	}
}

// The documented way to feed these scripts is a .env file, so that file must be
// impossible to commit by accident: it holds the admin password, the CSRF secret
// and the mongo URI with its credentials.
func TestSecretFilesAndRuntimeFilesAreIgnored(t *testing.T) {
	gitignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	ignored := map[string]bool{}
	for _, line := range strings.Split(string(gitignore), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			ignored[line] = true
		}
	}
	for _, want := range []string{".env", "/run/", "/rssfetcher"} {
		if !ignored[want] {
			t.Errorf(".gitignore does not ignore %s", want)
		}
	}
}

// A typo in a start script is only discovered when the server fails to come up.
func TestStartScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, name := range []string{"start-local.sh", "start-prod.sh"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("bash", "-n", name).CombinedOutput()
			if err != nil {
				t.Fatalf("%s does not parse: %v\n%s", name, err, out)
			}
		})
	}
}

// argv is world readable: any local user sees it in ps(1) output and in
// /proc/<pid>/cmdline, and it is commonly copied into bug reports and crash
// dumps. The scripts therefore hand the credentials to the binary through the
// environment (which is only readable by the same user) instead of as flags.
func TestStartScriptsKeepSecretsOutOfArgv(t *testing.T) {
	secretFlags := []string{
		"-admin-user",
		"-admin-password",
		"-auth-secret",
		"-address", // a mongo URI carries its own user:password
	}
	for _, name := range []string{"start-local.sh", "start-prod.sh"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				switch {
				case strings.HasPrefix(strings.TrimSpace(line), "#"):
					continue // documentation of the env vars is fine
				case strings.Contains(line, "envOr"), strings.Contains(line, ":?"):
					continue // existence checks do not put the value anywhere
				}
				for _, flag := range secretFlags {
					if strings.Contains(line, flag) && strings.Contains(line, "$") {
						t.Errorf("%s passes a secret through %s: %s", name, flag, strings.TrimSpace(line))
					}
				}
			}
			// Leaving the values out of argv only helps if they reach the child
			// some other way: a variable that is merely assigned inside the script
			// is not inherited, so each one has to be exported.
			exported := map[string]bool{}
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 1 && fields[0] == "export" {
					for _, variable := range fields[1:] {
						exported[strings.TrimSuffix(variable, ";")] = true
					}
				}
			}
			for _, variable := range []string{"MONGO_URI", "RSSFETCHER_ADMIN_USER", "RSSFETCHER_ADMIN_PASSWORD", "RSSFETCHER_AUTH_SECRET"} {
				if !exported[variable] {
					t.Errorf("%s does not export %s, so the binary would not see it", name, variable)
				}
			}
		})
	}
}
