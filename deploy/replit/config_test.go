package replitconfig

import (
	"os"
	"strings"
	"testing"
)

func TestReplitConfigurationBuildsOneHTTPService(t *testing.T) {
	data, err := os.ReadFile("../../.replit")
	if err != nil {
		t.Fatalf("read Replit configuration: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		`modules = ["go-1.25", "nodejs-22"]`,
		`build = "bash scripts/replit-build.sh"`,
		`run = "bash scripts/replit-run.sh"`,
		"localPort = 8080",
		"externalPort = 80",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Replit configuration missing %q", required)
		}
	}
	if strings.Contains(strings.ToLower(text), "docker") {
		t.Fatal("Replit deployment must use reviewed language modules instead of a nested Docker runtime")
	}
}

func TestReplitBuildUsesLockedDependenciesAndSingleBinary(t *testing.T) {
	buildData, err := os.ReadFile("../../scripts/replit-build.sh")
	if err != nil {
		t.Fatalf("read Replit build script: %v", err)
	}
	build := string(buildData)
	for _, required := range []string{
		"npm --prefix web ci --ignore-scripts --no-audit --no-fund",
		"npm --prefix web run build",
		"CGO_ENABLED=0 go build -trimpath",
	} {
		if !strings.Contains(build, required) {
			t.Fatalf("Replit build target missing %q", required)
		}
	}

	runData, err := os.ReadFile("../../scripts/replit-run.sh")
	if err != nil {
		t.Fatalf("read Replit run script: %v", err)
	}
	run := string(runData)
	for _, required := range []string{
		"exec ./bin/ember-control-api",
		"--listen-address=0.0.0.0:8080",
		"--secure-cookies=true",
		"--web-root=web/dist",
	} {
		if !strings.Contains(run, required) {
			t.Fatalf("Replit run target missing %q", required)
		}
	}
	if strings.Contains(run, "--secure-cookies=false") {
		t.Fatal("Replit runtime must not disable secure cookies")
	}
}
