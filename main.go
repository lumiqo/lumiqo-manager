package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Replaced at build time for versioned releases.
var version = "dev"

const usage = `Usage: lumiqo-manager <command> [options]

  status                 Report OS, WSL, tools and Worker state
  doctor [--json]        Check Ubuntu, systemd and setup prerequisites
  tools list [--json]    Report which supported tools are installed
  worker status          Report the Worker systemd unit
  setup [--plan|--apply]        Check prerequisites, optionally install missing apt packages
  auth                   Report sign-in state for supported CLI tools
  auth --login TOOL      Start interactive sign-in (gh, ntn, gcloud, codex)
  services [--user]      Report the Worker systemd unit
  version                Report the Manager version

Reports are JSON. --unit selects the Worker unit for status and services.
`

type systemInfo struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Distro  string `json:"distro"`
	Release string `json:"release"`
	WSL     bool   `json:"wsl"`
}

type service struct {
	Unit   string `json:"unit"`
	Scope  string `json:"scope"`
	Load   string `json:"load"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
	Error  string `json:"error,omitempty"`
}

type tool struct {
	name  string
	probe []string
	login []string
}

var authTools = []tool{
	{"gh", []string{"auth", "status", "--hostname", "github.com"}, []string{"auth", "login", "--hostname", "github.com", "--web"}},
	{"ntn", []string{"whoami"}, []string{"login"}},
	{"gcloud", []string{"auth", "list", "--filter=status:ACTIVE", "--format=value(account)"}, []string{"auth", "login"}},
	{"codex", []string{"login", "status"}, []string{"login"}},
}

var packages = []string{"ca-certificates", "curl", "git", "jq", "unzip"}

func host() systemInfo {
	info := systemInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	info.WSL = os.Getenv("WSL_DISTRO_NAME") != ""
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		info.WSL = info.WSL || strings.Contains(strings.ToLower(string(kernel)), "microsoft")
	}
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return info
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), "=")
		switch key {
		case "ID":
			info.Distro = strings.Trim(value, `"'`)
		case "VERSION_ID":
			info.Release = strings.Trim(value, `"'`)
		}
	}
	return info
}

func probe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return output, err
}

func worker(unit string, user bool) service {
	state := service{Unit: unit, Scope: "system", Load: "unknown", Active: "unknown", Sub: "unknown"}
	var args []string
	if user {
		args = append(args, "--user")
		state.Scope = "user"
	}
	args = append(args, "show", "--property=LoadState,ActiveState,SubState", "--", unit)
	output, err := probe("systemctl", args...)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "LoadState":
			state.Load = value
		case "ActiveState":
			state.Active = value
		case "SubState":
			state.Sub = value
		}
	}
	if err != nil && state.Load == "unknown" {
		state.Error = "systemd query failed: " + err.Error()
	}
	return state
}

func installed(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func authState(t tool) string {
	if !installed(t.name) {
		return "missing"
	}
	output, err := probe(t.name, t.probe...)
	if err != nil {
		return "unverified"
	}
	if t.name == "gcloud" && strings.TrimSpace(string(output)) == "" {
		return "signed_out"
	}
	return "signed_in"
}

func interactive(stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func missingPackages() []string {
	missing := []string{}
	for _, pkg := range packages {
		output, err := probe("dpkg-query", "-W", "-f=${Status}", pkg)
		if err != nil || string(output) != "install ok installed" {
			missing = append(missing, pkg)
		}
	}
	return missing
}

func setup(apply bool, stdin io.Reader, stderr io.Writer) (any, error) {
	info := host()
	if info.Distro != "ubuntu" || info.Release != "24.04" {
		return nil, errors.New("setup requires Ubuntu 24.04")
	}
	missing := missingPackages()
	if apply && len(missing) > 0 {
		command := "apt-get"
		var prefix []string
		if os.Geteuid() != 0 {
			command = "sudo"
			prefix = []string{"apt-get"}
		}
		for _, args := range [][]string{{"update"}, append([]string{"install", "-y", "--"}, missing...)} {
			if err := interactive(stdin, stderr, stderr, command, append(prefix, args...)...); err != nil {
				return nil, fmt.Errorf("apt-get %s: %w", args[0], err)
			}
		}
		missing = missingPackages()
		if len(missing) > 0 {
			return nil, fmt.Errorf("packages still missing: %s", strings.Join(missing, ", "))
		}
	}
	return map[string]any{"system": info, "applied": apply, "missing_packages": missing}, nil
}

func installedTools() map[string]bool {
	tools := map[string]bool{}
	for _, name := range []string{"git", "gh", "ntn", "gcloud", "codex", "mise", "uv", "docker"} {
		tools[name] = installed(name)
	}
	return tools
}

func doctor(unit string, user bool) any {
	info := host()
	systemd, err := os.ReadFile("/proc/1/comm")
	return map[string]any{
		"system":           info,
		"supported_ubuntu": info.Distro == "ubuntu" && info.Release == "24.04",
		"systemd":          err == nil && strings.TrimSpace(string(systemd)) == "systemd",
		"missing_packages": missingPackages(),
		"tools":            installedTools(),
		"worker":           worker(unit, user),
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprint(stdout, usage)
		return err
	}
	command := args[0]
	options := args[1:]
	switch command {
	case "tools", "worker":
		subcommand := map[string]string{"tools": "list", "worker": "status"}[command]
		if len(options) == 0 || options[0] != subcommand {
			return fmt.Errorf("expected %s %s", command, subcommand)
		}
		options = options[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	switch command {
	case "status", "services", "doctor", "worker", "tools", "setup":
		flags.Bool("json", false, "emit JSON (default)")
	}
	var unit, login string
	var apply, plan, user bool
	switch command {
	case "status", "services", "doctor", "worker":
		flags.StringVar(&unit, "unit", "lumiqo-worker.service", "Worker systemd unit")
		flags.BoolVar(&user, "user", false, "query the user systemd manager")
	case "tools":
	case "setup":
		flags.BoolVar(&plan, "plan", false, "report missing apt packages without installing")
		flags.BoolVar(&apply, "apply", false, "install missing apt packages")
	case "auth":
		flags.StringVar(&login, "login", "", "start interactive sign-in for TOOL")
	case "version":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err := flags.Parse(options); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	var result any
	switch command {
	case "status":
		result = map[string]any{"version": version, "system": host(), "tools": installedTools(), "worker": worker(unit, user)}
	case "services", "worker":
		result = worker(unit, user)
	case "doctor":
		result = doctor(unit, user)
	case "tools":
		result = installedTools()
	case "setup":
		if plan && apply {
			return errors.New("--plan and --apply are mutually exclusive")
		}
		report, err := setup(apply, stdin, stderr)
		if err != nil {
			return err
		}
		result = report
	case "auth":
		if login != "" {
			for _, t := range authTools {
				if t.name == login {
					return interactive(stdin, stdout, stderr, t.name, t.login...)
				}
			}
			return fmt.Errorf("unsupported sign-in tool %q", login)
		}
		states := map[string]string{}
		for _, t := range authTools {
			states[t.name] = authState(t)
		}
		result = states
	case "version":
		result = map[string]string{"version": version}
	}
	return json.NewEncoder(stdout).Encode(result)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}
