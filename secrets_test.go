package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSecretFilesAreClosedToFileTools(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	for name, content := range map[string]string{".env": "OPENAI_API_KEY=sk-secret\n", ".env.local": "TOKEN=sk-local\n", ".env.example": "OPENAI_API_KEY=\n", "notes.txt": "sk-secret is not here\n"} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{".env", ".env.local", "./.ENV", "sub/../.env"} {
		if _, err := readFile(name); err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Errorf("%s: expected the file to be refused, got %v", name, err)
		}
	}
	if _, err := readFile(".env.example"); err != nil {
		t.Errorf("expected .env.example to stay readable, got %v", err)
	}
	if err := os.Symlink(".env", "innocent.txt"); err == nil {
		if _, err := readFile("innocent.txt"); err == nil {
			t.Error("expected a symlink to .env to be refused")
		}
	}

	matches, _, err := grepFiles(".", "sk-", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if strings.HasPrefix(match, ".env") {
			t.Errorf("expected grep to skip secret files, got %q", match)
		}
	}
}

func TestRunBashDoesNotInheritCredentials(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("SOME_SERVICE_API_KEY", "sk-other")
	t.Setenv("FROM_DOTENV", "dotenv-value")
	t.Setenv("GOAT_HARMLESS", "visible")
	dotenvKeys["FROM_DOTENV"] = true
	t.Cleanup(func() { delete(dotenvKeys, "FROM_DOTENV") })

	output, err := runBashCommand(context.Background(), "env", ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-openai", "sk-other", "dotenv-value"} {
		if strings.Contains(output, secret) {
			t.Errorf("expected %s to be scrubbed from the command environment", secret)
		}
	}
	if !strings.Contains(output, "GOAT_HARMLESS=visible") {
		t.Error("expected ordinary variables to be passed on")
	}
}

func TestStateDirCommandsAlwaysAsk(t *testing.T) {
	approver := &bashApprover{autoApprove: true}
	if err := approver.approve(context.Background(), "ls", "."); err != nil {
		t.Fatalf("expected an ordinary command to run, got %v", err)
	}
	if err := approver.approve(context.Background(), "rm -rf .goat/snapshots", "."); err == nil {
		t.Fatal("expected a command touching .goat to need approval")
	}
}
