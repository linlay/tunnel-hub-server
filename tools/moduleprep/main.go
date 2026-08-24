package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const modulePathEnv = "GO_MODULE_PATH"

var modulePathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9][A-Za-z0-9._~+-]*)+$`)
var moduleHostLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "moduleprep:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected exec, run, or rewrite command")
	}
	switch args[0] {
	case "exec":
		return runExec(args[1:])
	case "run":
		return runBinary(args[1:])
	case "rewrite":
		return runRewrite(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runExec(args []string) error {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	modulePath := flags.String("module", "", "target Go module path")
	envFile := flags.String("env-file", ".env", "dotenv file used when GO_MODULE_PATH is not exported")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command := flags.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return errors.New("exec requires a command after --")
	}

	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	target, err := resolveModulePath(*modulePath, envFilePath(root, *envFile))
	if err != nil {
		return err
	}
	tempRoot, cleanup, err := prepareTemporaryTree(root, target)
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = tempRoot
	cmd.Env = withEnvironment(os.Environ(), "GOTOOLCHAIN", "local")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runBinary(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	modulePath := flags.String("module", "", "target Go module path")
	envFile := flags.String("env-file", ".env", "dotenv file used when GO_MODULE_PATH is not exported")
	packagePath := flags.String("package", "", "main package to build and run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	programArgs := flags.Args()
	if len(programArgs) > 0 && programArgs[0] == "--" {
		programArgs = programArgs[1:]
	}
	if strings.TrimSpace(*packagePath) == "" {
		return errors.New("run requires -package")
	}

	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	target, err := resolveModulePath(*modulePath, envFilePath(root, *envFile))
	if err != nil {
		return err
	}
	tempRoot, cleanup, err := prepareTemporaryTree(root, target)
	if err != nil {
		return err
	}
	defer cleanup()

	binaryPath := filepath.Join(tempRoot, ".moduleprep-program")
	build := exec.Command("go", "build", "-o", binaryPath, *packagePath)
	build.Dir = tempRoot
	build.Env = withEnvironment(os.Environ(), "GOTOOLCHAIN", "local")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return err
	}

	program := exec.Command(binaryPath, programArgs...)
	program.Dir = root
	program.Env = os.Environ()
	program.Stdin = os.Stdin
	program.Stdout = os.Stdout
	program.Stderr = os.Stderr
	return program.Run()
}

func runRewrite(args []string) error {
	flags := flag.NewFlagSet("rewrite", flag.ContinueOnError)
	modulePath := flags.String("module", "", "target Go module path")
	root := flags.String("root", ".", "source tree to rewrite")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target, err := resolveModulePath(*modulePath, "")
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	return rewriteModule(absRoot, target)
}

func repositoryRoot() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func resolveModulePath(flagValue, envFile string) (string, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" {
		value = strings.TrimSpace(os.Getenv(modulePathEnv))
	}
	if value == "" && envFile != "" {
		loaded, err := readDotEnvValue(envFile, modulePathEnv)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(loaded)
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", modulePathEnv)
	}
	if !validModulePath(value) {
		return "", fmt.Errorf("%s is invalid: %q", modulePathEnv, value)
	}
	return value, nil
}

func envFilePath(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(root, name)
}

func validModulePath(value string) bool {
	if strings.Contains(value, "://") || strings.Contains(value, "//") || strings.HasSuffix(value, "/") {
		return false
	}
	first, _, _ := strings.Cut(value, "/")
	labels := strings.Split(first, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !moduleHostLabelPattern.MatchString(label) {
			return false
		}
	}
	return modulePathPattern.MatchString(value)
}

func readDotEnvValue(path, wantedKey string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != wantedKey {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

func prepareTemporaryTree(sourceRoot, modulePath string) (string, func(), error) {
	tempRoot, err := os.MkdirTemp("", "tunnel-moduleprep-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempRoot) }
	if err := copyWorktree(sourceRoot, tempRoot); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := rewriteModule(tempRoot, modulePath); err != nil {
		cleanup()
		return "", nil, err
	}
	return tempRoot, cleanup, nil
}

func copyWorktree(sourceRoot, destinationRoot string) error {
	command := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	command.Dir = sourceRoot
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("list worktree files: %w", err)
	}
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		relativePath := string(rawPath)
		if excludedWorktreePath(relativePath) {
			continue
		}
		err := copyFile(filepath.Join(sourceRoot, relativePath), filepath.Join(destinationRoot, relativePath))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", relativePath, err)
		}
	}
	return nil
}

func excludedWorktreePath(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	base := filepath.Base(cleaned)
	if cleaned == ".env" || strings.HasPrefix(cleaned, ".local/") || hasDirectory(cleaned, "node_modules") || hasDirectory(cleaned, "dist") || hasDirectory(cleaned, "build") || hasDirectory(cleaned, "bin") {
		return true
	}
	extension := strings.ToLower(filepath.Ext(base))
	return extension == ".pem" || extension == ".key" || extension == ".db" || extension == ".sqlite" || extension == ".sqlite3"
}

func hasDirectory(path, directory string) bool {
	return strings.HasPrefix(path, directory+"/") || strings.Contains(path, "/"+directory+"/")
}

func copyFile(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not supported in the temporary build tree")
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func rewriteModule(root, targetModule string) error {
	goModPath := filepath.Join(root, "go.mod")
	sourceModule, err := readModuleDirective(goModPath)
	if err != nil {
		return err
	}
	if sourceModule == targetModule {
		return nil
	}

	if err := rewriteFile(goModPath, []byte("module "+sourceModule), []byte("module "+targetModule)); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "dist" || entry.Name() == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		return rewriteFile(path, []byte(sourceModule), []byte(targetModule))
	})
}

func readModuleDirective(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "module "))
			if !validModulePath(value) {
				return "", fmt.Errorf("source module path is invalid: %q", value)
			}
			return value, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("module directive not found in %s", path)
}

func rewriteFile(path string, oldValue, newValue []byte) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated := bytes.ReplaceAll(contents, oldValue, newValue)
	if bytes.Equal(contents, updated) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporaryPath := path + ".moduleprep-tmp"
	if err := os.WriteFile(temporaryPath, updated, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func withEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			updated = append(updated, item)
		}
	}
	return append(updated, prefix+value)
}
