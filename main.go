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

  status                 Report OS, WSL, tools and distribution state
  doctor [--json]        Check Ubuntu, systemd and setup prerequisites
  tools list [--json]    Report which supported tools are installed
  setup [--plan|--apply]        Check prerequisites, optionally install missing apt packages
  install lumiqot-linux  Install the optional Lumiqot distribution
  rollback               Activate the retained previous package
  reconcile --plan       Report environment differences as JSON
  reconcile --apply [--restore]
                         Repair the environment and update installed components
  auth                   Report sign-in state for supported CLI tools
  auth --login TOOL      Start interactive sign-in (gh, ntn, gcloud, codex)
  services               Report desired state, runtime status and health
  version                Report the Manager version

Reports are JSON.
`

type systemInfo struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Distro  string `json:"distro"`
	Release string `json:"release"`
	WSL     bool   `json:"wsl"`
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

var packages = []string{
	"7zip", "bat", "bubblewrap", "build-essential", "ca-certificates", "csvkit", "curl",
	"fd-find", "ffmpeg", "file", "git", "git-lfs", "gpg", "imagemagick",
	"jq", "libimage-exiftool-perl", "miller", "pandoc", "poppler-utils",
	"python3", "python3-venv", "ripgrep", "rsync", "shellcheck", "sqlite3",
	"tar", "tree", "unzip", "wget", "yq", "zstd",
}

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

func missingPackages(extra ...string) []string {
	return missingAptPackages(append(append([]string{}, packages...), extra...))
}

func missingAptPackages(requested []string) []string {
	missing := []string{}
	for _, pkg := range requested {
		output, err := probe("dpkg-query", "-W", "-f=${Status}", pkg)
		if err != nil || !strings.HasSuffix(string(output), " ok installed") {
			missing = append(missing, pkg)
		}
	}
	return missing
}

func setup(apply bool, stdin io.Reader, stderr io.Writer, extra ...string) (any, error) {
	info := host()
	if info.Distro != "ubuntu" || info.Release != "24.04" {
		return nil, errors.New("setup requires Ubuntu 24.04")
	}
	missing := missingPackages(extra...)
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
		missing = missingPackages(extra...)
		if len(missing) > 0 {
			return nil, fmt.Errorf("packages still missing: %s", strings.Join(missing, ", "))
		}
	}
	ghReady := githubCLIReady()
	if apply && !ghReady {
		if err := installGitHubCLI(stdin, stderr); err != nil {
			return nil, err
		}
		ghReady = true
	}
	return map[string]any{"system": info, "applied": apply, "missing_packages": missing, "github_cli_ready": ghReady}, nil
}

func installedTools() map[string]bool {
	tools := map[string]bool{}
	for _, name := range []string{"git", "gh", "ntn", "gcloud", "codex", "mise", "uv", "docker", "node", "npm", "duckdb", "python-analysis"} {
		tools[name] = installed(name)
	}
	return tools
}

func doctor() any {
	info := host()
	systemd, err := os.ReadFile("/proc/1/comm")
	return map[string]any{
		"system":           info,
		"supported_ubuntu": info.Distro == "ubuntu" && info.Release == "24.04",
		"systemd":          err == nil && strings.TrimSpace(string(systemd)) == "systemd",
		"missing_packages": missingPackages(),
		"tools":            installedTools(),
		"environment":      environmentHealth(),
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
	case "tools":
		subcommand := "list"
		if len(options) == 0 || options[0] != subcommand {
			return fmt.Errorf("expected %s %s", command, subcommand)
		}
		options = options[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	switch command {
	case "status", "services", "doctor", "tools", "setup":
		flags.Bool("json", false, "emit JSON (default)")
	}
	var login, archiveVersion, archiveHash string
	var apply, plan, restore bool
	switch command {
	case "status", "services", "doctor", "tools":
	case "setup":
		flags.BoolVar(&plan, "plan", false, "report missing apt packages without installing")
		flags.BoolVar(&apply, "apply", false, "install missing apt packages")
	case "reconcile":
		flags.BoolVar(&plan, "plan", false, "report differences without changing the environment")
		flags.BoolVar(&apply, "apply", false, "apply differences")
		flags.BoolVar(&restore, "restore", false, "also reinstall previously installed recommended components")
	case "auth":
		flags.StringVar(&login, "login", "", "start interactive sign-in for TOOL")
	case "install":
		flags.StringVar(&archiveVersion, "archive-version", "", "install a local archive from stdin with this version")
		flags.StringVar(&archiveHash, "archive-sha256", "", "expected SHA-256 of the local archive")
		flags.Usage = func() {
			fmt.Fprintln(stderr, "Usage: lumiqo-manager install [--archive-version VERSION --archive-sha256 HASH] lumiqot-linux")
		}
	case "version", "rollback":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err := flags.Parse(options); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command == "install" {
		if flags.NArg() != 1 {
			return errors.New("expected install lumiqot-linux")
		}
		if archiveVersion != "" || archiveHash != "" {
			if flags.Arg(0) != distributionRepo {
				return errors.New("local archive requires lumiqot-linux")
			}
			report, err := installLocalDistribution(archiveVersion, archiveHash, stdin)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(report)
		}
		report, err := installOptional(flags.Arg(0), stdin, stderr)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(report)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	var result any
	switch command {
	case "status", "services":
		distribution, err := managedDistribution().report()
		if err != nil {
			return err
		}
		result = distribution
		if command == "status" {
			result = map[string]any{"version": version, "system": host(), "tools": installedTools(), "distribution": distribution}
		}
	case "doctor":
		result = doctor()
	case "rollback":
		if err := rollbackDistribution(); err != nil {
			return err
		}
		report, err := managedDistribution().report()
		if err != nil {
			return err
		}
		result = report
	case "tools":
		result = installedTools()
	case "reconcile":
		if plan == apply || (restore && !apply) {
			return errors.New("specify exactly one of --plan and --apply; --restore requires --apply")
		}
		report, err := reconcile(apply, restore, "", stdin, stderr)
		if err != nil {
			return err
		}
		result = report
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
