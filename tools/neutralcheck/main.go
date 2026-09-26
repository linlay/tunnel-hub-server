package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const forbiddenTermsEnv = "FORBIDDEN_BRAND_TERMS"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "neutralcheck:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("neutralcheck", flag.ContinueOnError)
	ref := flags.String("ref", "", "Git tree to scan instead of the working tree")
	envFile := flags.String("env-file", ".env", "dotenv file used when FORBIDDEN_BRAND_TERMS is not exported")
	if err := flags.Parse(args); err != nil {
		return err
	}
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	rawTerms := strings.TrimSpace(os.Getenv(forbiddenTermsEnv))
	if rawTerms == "" {
		rawTerms, err = readDotEnvValue(envFilePath(root, *envFile), forbiddenTermsEnv)
		if err != nil {
			return err
		}
	}
	terms := splitTerms(rawTerms)
	if len(terms) == 0 {
		return fmt.Errorf("%s is required", forbiddenTermsEnv)
	}
	return scan(root, *ref, terms)
}

func envFilePath(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(root, name)
}

func repositoryRoot() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func splitTerms(raw string) [][]byte {
	seen := make(map[string]struct{})
	var terms [][]byte
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		term := strings.ToLower(strings.TrimSpace(field))
		if term == "" {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, []byte(term))
	}
	return terms
}

func scan(root, ref string, terms [][]byte) error {
	files, err := trackedFiles(root, ref)
	if err != nil {
		return err
	}
	matchCount := 0
	for _, path := range files {
		lowerPath := bytes.ToLower([]byte(filepath.ToSlash(path)))
		for _, term := range terms {
			if bytes.Contains(lowerPath, term) {
				fmt.Fprintf(os.Stderr, "forbidden term found in path: %s\n", path)
				matchCount++
				break
			}
		}
		contents, err := trackedFileContents(root, ref, path)
		if err != nil {
			return err
		}
		lowerContents := bytes.ToLower(contents)
		for _, term := range terms {
			if bytes.Contains(lowerContents, term) {
				fmt.Fprintf(os.Stderr, "forbidden term found in content: %s\n", path)
				matchCount++
				break
			}
		}
	}
	if matchCount > 0 {
		return fmt.Errorf("found %d forbidden-term matches in repository files", matchCount)
	}
	return nil
}

func trackedFiles(root, ref string) ([]string, error) {
	var command *exec.Cmd
	if ref == "" {
		command = exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	} else {
		command = exec.Command("git", "ls-tree", "-r", "-z", "--name-only", ref)
	}
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}
	var files []string
	for _, item := range bytes.Split(output, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		path := string(item)
		if ref == "" {
			if _, err := os.Lstat(filepath.Join(root, path)); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("inspect %s: %w", path, err)
			}
		}
		files = append(files, path)
	}
	return files, nil
}

func trackedFileContents(root, ref, path string) ([]byte, error) {
	if ref == "" {
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return contents, nil
	}
	command := exec.Command("git", "show", ref+":"+path)
	command.Dir = root
	contents, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", path, ref, err)
	}
	return contents, nil
}

func readDotEnvValue(path, wantedKey string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != wantedKey {
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
