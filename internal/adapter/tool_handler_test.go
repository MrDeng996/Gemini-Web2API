package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseToolCallsBasic(t *testing.T) {
	input := "Before\n\n```json action\n{\n  \"tool\": \"read_file\",\n  \"parameters\": {\n    \"path\": \"README.md\"\n  }\n}\n```\n\nAfter"

	calls, clean := parseToolCalls(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("expected read_file, got %q", calls[0].Name)
	}
	if got, _ := calls[0].Arguments["path"].(string); got != "README.md" {
		t.Fatalf("expected path README.md, got %q", got)
	}
	if strings.Contains(clean, "```json") {
		t.Fatalf("expected clean text without tool block, got %q", clean)
	}
	if !strings.Contains(clean, "Before") || !strings.Contains(clean, "After") {
		t.Fatalf("expected surrounding text preserved, got %q", clean)
	}
}

func TestParseToolCallsSmartQuotes(t *testing.T) {
	input := "```json action\n{\n  “tool”: “read_file”,\n  “parameters”: {\n    “path”: “README.md”\n  }\n}\n```"

	calls, _ := parseToolCalls(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("expected read_file, got %q", calls[0].Name)
	}
}

func TestParseToolCallsTruncatedJSON(t *testing.T) {
	input := "```json action\n{\n  \"tool\": \"write_to_file\",\n  \"parameters\": {\n    \"path\": \"a.txt\",\n    \"content\": \"hello\"\n  }"

	calls, _ := parseToolCalls(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call from truncated block, got %d", len(calls))
	}
	if calls[0].Name != "write_to_file" {
		t.Fatalf("expected write_to_file, got %q", calls[0].Name)
	}
}

func TestParseToolCallsGreedyLargeField(t *testing.T) {
	input := "```json action\n{\n  \"tool\": \"write_to_file\",\n  \"parameters\": {\n    \"path\": \"main.go\",\n    \"content\": \"package main\\n\\nfunc main() {\\n  println(\\\"hello\\\")\\n}\\n\"\n  }\n}\n```"

	calls, _ := parseToolCalls(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	content, _ := calls[0].Arguments["content"].(string)
	if !strings.Contains(content, `println("hello")`) {
		t.Fatalf("expected content to contain decoded code, got %q", content)
	}
}

func TestRepairExactMatchToolArguments(t *testing.T) {
	t.Setenv("TOOL_ARG_REPAIR_ENABLED", "true")
	t.Setenv("TOOL_ARG_REPAIR_MAX_FILE_SIZE", "4096")

	dir := t.TempDir()
	filePath := filepath.Join(dir, "sample.txt")
	content := "fmt.Println(“hello”)\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	args := map[string]interface{}{
		"path":       filePath,
		"old_string": `fmt.Println("hello")`,
		"new_string": "fmt.Println(“world”)",
	}

	fixed := repairExactMatchToolArguments("search_replace", args)
	oldString, _ := fixed["old_string"].(string)
	newString, _ := fixed["new_string"].(string)

	if oldString != "fmt.Println(“hello”)" {
		t.Fatalf("expected old_string repaired to exact file text, got %q", oldString)
	}
	if newString != `fmt.Println("world")` {
		t.Fatalf("expected new_string smart quotes normalized, got %q", newString)
	}
}

func TestCompactSchema(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
			"recursive": map[string]interface{}{"type": "boolean"},
		},
		"required": []interface{}{"path"},
	}

	got := compactSchema(schema)
	if !strings.Contains(got, "path!: string") {
		t.Fatalf("expected compact schema to mark required path, got %q", got)
	}
	if !strings.Contains(got, "recursive?: boolean") {
		t.Fatalf("expected compact schema to include optional recursive, got %q", got)
	}
}
