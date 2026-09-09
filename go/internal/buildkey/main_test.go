package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotenvHandlesSupportedSyntaxLiterally(t *testing.T) {
	values, err := parseDotenv("\n# comment\nexport GEMINI_API_KEY = 'single # literal' # comment\nNANOBANANA_BUNDLE_KEY=\"$dollar `backtick` # literal\"\nUNRELATED=$(never-run)\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := values[geminiKeyName]; got != "single # literal" {
		t.Fatalf("Gemini key = %q", got)
	}
	if got := values[bundleKeyName]; got != "$dollar `backtick` # literal" {
		t.Fatalf("bundle key = %q", got)
	}
}

func TestParseDotenvRejectsMalformedKeyWithoutLeakingValue(t *testing.T) {
	_, err := parseDotenv("GEMINI_API_KEY='private-value")
	if err == nil || strings.Contains(err.Error(), "private-value") || !strings.Contains(err.Error(), geminiKeyName) {
		t.Fatalf("error = %v", err)
	}
}

func TestFindKeyPrecedence(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte("GEMINI_API_KEY=first-gemini\nNANOBANANA_BUNDLE_KEY=first-bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("NANOBANANA_BUNDLE_KEY=second-bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bundleKeyName, "")
	t.Setenv(geminiKeyName, "")

	key, source, found, err := findKey([]string{first, second})
	if err != nil || !found || key != "first-bundle" || source != first {
		t.Fatalf("file key = %q, source = %q, found = %t, err = %v", key, source, found, err)
	}
	t.Setenv(geminiKeyName, "environment-gemini")
	key, source, found, err = findKey([]string{first})
	if err != nil || !found || key != "environment-gemini" || source != "environment "+geminiKeyName {
		t.Fatalf("Gemini environment key = %q, source = %q, found = %t, err = %v", key, source, found, err)
	}
	t.Setenv(bundleKeyName, "environment-bundle")
	key, source, found, err = findKey([]string{first})
	if err != nil || !found || key != "environment-bundle" || source != "environment "+bundleKeyName {
		t.Fatalf("bundle environment key = %q, source = %q, found = %t, err = %v", key, source, found, err)
	}
}

func TestRunWritesPrivateGeneratedSource(t *testing.T) {
	dir := t.TempDir()
	dotenv := filepath.Join(dir, ".env")
	output := filepath.Join(dir, "generated", "bundled_key.go")
	if err := os.WriteFile(dotenv, []byte("GEMINI_API_KEY=dummy-secret # comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bundleKeyName, "")
	t.Setenv(geminiKeyName, "")
	var log strings.Builder
	buildOutput = &log
	t.Cleanup(func() { buildOutput = os.Stdout })
	if err := run([]string{output, dotenv}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `bundledAPIKey = "dummy-secret"`) {
		t.Fatalf("generated source did not contain expected assignment")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), output, data, parser.AllErrors); err != nil {
		t.Fatalf("generated source is not valid Go: %v", err)
	}
	if got := log.String(); strings.Contains(got, "dummy-secret") || !strings.Contains(got, dotenv) {
		t.Fatalf("unsafe or unhelpful output = %q", got)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRunDoesNotWriteWithoutKey(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "bundled_key.go")
	t.Setenv(bundleKeyName, "")
	t.Setenv(geminiKeyName, "")
	var log strings.Builder
	buildOutput = &log
	t.Cleanup(func() { buildOutput = os.Stdout })
	if err := run([]string{output, filepath.Join(dir, "missing.env")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output stat error = %v", err)
	}
	if got := log.String(); got != "No API key bundled; set GEMINI_API_KEY at runtime.\n" {
		t.Fatalf("message = %q", got)
	}
}
